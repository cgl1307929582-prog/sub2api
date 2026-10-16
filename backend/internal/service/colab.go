package service

import (
	"net/url"
	"regexp"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func (a *Account) IsColab() bool { return a != nil && a.Platform == PlatformColab }

var colabAPIKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// Colab is a self-hosted Pool. Never fall back to another provider's URL or
// accept administrative OAuth credentials for this inference-only integration.
func validateColabAccount(a *Account) error {
	if !a.IsColab() {
		return nil
	}
	if a.Type != AccountTypeAPIKey {
		return infraerrors.BadRequest("INVALID_COLAB_ACCOUNT", "Colab GPU Pool requires API key credentials")
	}
	key := a.GetCredential("api_key")
	if len(key) < 8 || len(key) > 8192 || !colabAPIKeyPattern.MatchString(key) {
		return infraerrors.BadRequest("INVALID_COLAB_KEY", "Colab Pool API key is missing or invalid")
	}
	base, err := url.Parse(a.GetOpenAIBaseURL())
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return infraerrors.BadRequest("INVALID_COLAB_URL", "Colab Pool requires an explicit HTTP(S) bridge base URL")
	}
	protocol := strings.TrimSpace(a.GetCredential("api_protocol"))
	if protocol != "" && protocol != APIProtocolAdaptive && protocol != APIProtocolChatCompletions {
		return infraerrors.BadRequest("INVALID_COLAB_PROTOCOL", "Colab Pool uses Chat Completions upstream; other client formats are converted automatically")
	}
	return nil
}
