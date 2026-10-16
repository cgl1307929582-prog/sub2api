package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func colabControlFixture(t *testing.T, worker http.HandlerFunc) (*ColabControlHandler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(worker)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "control-secret")
	require.NoError(t, os.WriteFile(path, []byte("private-worker-secret-123456789"), 0600))
	h := NewColabControlHandler(&config.Config{ColabPool: config.ColabPoolConfig{ControlURL: server.URL + "/control", ControlSecretFile: path}})
	r := gin.New()
	r.GET("/snapshot", h.Snapshot)
	r.POST("/auth/start", h.AuthStart)
	r.GET("/auth/pending/:pid", h.AuthPending)
	r.POST("/auth/finish", h.AuthFinish)
	r.PATCH("/accounts/:id", h.UpdateAccount)
	r.DELETE("/accounts/:id", h.DeleteAccount)
	r.POST("/instances", h.StartInstance)
	r.POST("/instances/:id/stop", h.StopInstance)
	r.PUT("/settings", h.UpdateSettings)
	return h, r
}
func colabRequest(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Auth", "browser-must-not-control-this")
	r.ServeHTTP(w, req)
	return w
}
func TestColabControlReadOnlySnapshotAndCredentialIsolation(t *testing.T) {
	var paths []string
	_, router := colabControlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "private-worker-secret-123456789", r.Header.Get("X-Internal-Auth"))
		require.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/control/state" {
			fmt.Fprint(w, `{"accounts":[{"id":1,"email":"example@example.test","balance":0,"credentials":{"refresh_token":"hidden"},"error":"private-worker-secret-123456789"}],"instances":[],"summary":{"ready":0}}`)
		} else {
			fmt.Fprint(w, `{"resources":{},"gateway":{"requests":99},"credentials":"hidden","state":{"bad":true}}`)
		}
	})
	w := colabRequest(router, "GET", "/snapshot", "")
	require.Equal(t, 200, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Equal(t, []string{"/control/state", "/control/telemetry"}, paths)
	require.NotContains(t, w.Body.String(), "private-worker-secret")
	require.NotContains(t, w.Body.String(), "credentials")
	require.NotContains(t, w.Body.String(), "gateway")
	require.Contains(t, w.Body.String(), "example@example.test")
	require.Contains(t, w.Body.String(), `"balance":0`)
}
func TestColabControlTelemetryFailurePreservesState(t *testing.T) {
	_, router := colabControlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "telemetry") {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"detail":"worker unavailable"}`)
			return
		}
		fmt.Fprint(w, `{"summary":{"ready":0}}`)
	})
	w := colabRequest(router, "GET", "/snapshot", "")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"telemetry_available":false`)
	require.Contains(t, w.Body.String(), `"ready":0`)
}
func TestColabControlRejectsRedirects(t *testing.T) {
	var leaked atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer sink.Close()
	_, router := colabControlFixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, sink.URL, http.StatusFound) })
	w := colabRequest(router, "GET", "/snapshot", "")
	require.GreaterOrEqual(t, w.Code, 500)
	require.False(t, leaked.Load())
}
func TestColabControlInvalidInputsNeverReachWorker(t *testing.T) {
	var requests atomic.Int64
	_, router := colabControlFixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); fmt.Fprint(w, `{}`) })
	cases := [][3]string{{"PATCH", "/accounts/-1", `{}`}, {"PATCH", "/accounts/1", `{"enabled":"false"}`}, {"PATCH", "/accounts/1", `{"api_key":"bad"}`}, {"PATCH", "/accounts/1", `{"priority":10001}`}, {"PATCH", "/accounts/1", `{} {}`}, {"POST", "/auth/finish", `{"pid":"../escape","code":"123456"}`}, {"GET", "/auth/pending/not-a-pid", ""}, {"POST", "/instances", `{"account_id":0}`}, {"POST", "/instances/1/stop", `{"drain":"false"}`}, {"PUT", "/settings", `{"max_instances":0}`}, {"PUT", "/settings", `{"max_instances":1.5}`}, {"PUT", "/settings", `{"auto_start":"false"}`}, {"PUT", "/settings", `{"gpu":"invalid"}`}, {"PUT", "/settings", `{"target_url":"http://evil"}`}, {"PATCH", "/accounts/1", `{"note":"` + strings.Repeat("a", 17000) + `"}`}}
	for _, c := range cases {
		t.Run(c[1]+c[2][:min(len(c[2]), 35)], func(t *testing.T) {
			w := colabRequest(router, c[0], c[1], c[2])
			require.Equal(t, 400, w.Code, w.Body.String())
		})
	}
	require.Zero(t, requests.Load())
}
func TestColabControlMutationContract(t *testing.T) {
	var method, path string
	var body map[string]any
	_, router := colabControlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		fmt.Fprint(w, `{"ok":true}`)
	})
	cases := [][4]string{{"PATCH", "/accounts/8", `{"enabled":false,"priority":4}`, "POST /control/accounts/8"}, {"DELETE", "/accounts/8", "", "DELETE /control/accounts/8"}, {"POST", "/instances/4/stop", `{"drain":true}`, "POST /control/instances/4/stop"}, {"PUT", "/settings", `{"auto_start":false,"max_instances":2,"gpu":"G4"}`, "POST /control/settings"}, {"POST", "/auth/finish", `{"pid":"0123456789abcdef0123456789abcdef","code":"private-code"}`, "POST /control/auth/finish"}}
	for _, c := range cases {
		w := colabRequest(router, c[0], c[1], c[2])
		require.Equal(t, 200, w.Code)
		require.Equal(t, c[3], method+" "+path)
	}
	require.Equal(t, "private-code", body["code"])
}
func TestColabControlStartRespectsCapacityAndAccountCredits(t *testing.T) {
	for _, tc := range []struct {
		name, state, request string
		want                 int
	}{{"full", `{"settings":{"max_instances":1},"instances":[{"state":"draining"}]}`, `{}`, 400}, {"low credits", `{"settings":{"max_instances":2,"min_start_cu":8,"gpu":"G4"},"instances":[],"accounts":[{"id":1,"enabled":1,"status":"ok","balance":7,"instance":null,"cooldown":false,"assignments":0,"gpus":["G4"]}]}`, `{"account_id":1}`, 400}, {"eligible", `{"settings":{"max_instances":2,"min_start_cu":8,"gpu":"G4"},"instances":[],"accounts":[{"id":1,"enabled":1,"status":"ok","balance":20,"instance":null,"cooldown":false,"assignments":0,"gpus":["G4"]}]}`, `{"account_id":1}`, 200}} {
		t.Run(tc.name, func(t *testing.T) {
			var starts atomic.Int64
			_, router := colabControlFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/control/state" {
					fmt.Fprint(w, tc.state)
				} else {
					starts.Add(1)
					fmt.Fprint(w, `{"id":42}`)
				}
			})
			w := colabRequest(router, "POST", "/instances", tc.request)
			require.Equal(t, tc.want, w.Code)
			if tc.want != 200 {
				require.Zero(t, starts.Load())
			} else {
				require.EqualValues(t, 1, starts.Load())
			}
		})
	}
}
func TestColabControlUnavailableAndErrorsDoNotLeakWorkerSecrets(t *testing.T) {
	_, router := colabControlFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"detail":"private-worker-secret-123456789 refresh_token=hidden"}`)
	})
	w := colabRequest(router, "POST", "/auth/start", "")
	require.Equal(t, 400, w.Code)
	require.NotContains(t, w.Body.String(), "private-worker-secret")
	require.NotContains(t, w.Body.String(), "hidden")
	h := NewColabControlHandler(nil)
	r := gin.New()
	r.GET("/snapshot", h.Snapshot)
	w = colabRequest(r, "GET", "/snapshot", "")
	require.Equal(t, 503, w.Code)
}
