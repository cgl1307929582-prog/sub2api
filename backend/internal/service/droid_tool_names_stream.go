package service

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type droidToolSSEFrame struct {
	raw, data, original []byte
	event               string
}

func (f *droidToolSSEFrame) setName(path, name string) error {
	updated, err := sjson.SetBytes(f.data, path, name)
	if err == nil {
		f.data = updated
	}
	return err
}

func (f *droidToolSSEFrame) render(names *droidToolNames) []byte {
	data := restoreDroidToolNamesJSON(f.data, names)
	if bytes.Equal(data, f.original) {
		return f.raw
	}
	// Keep event/id/retry/comment lines and their line endings. A changed
	// multiline data payload becomes one JSON data line in the same event.
	var out []byte
	written := false
	for _, line := range bytes.SplitAfter(f.raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			out = append(out, line...)
			continue
		}
		if written {
			continue
		}
		written = true
		ending := []byte(nil)
		if bytes.HasSuffix(line, []byte("\r\n")) {
			ending = []byte("\r\n")
		} else if bytes.HasSuffix(line, []byte("\n")) {
			ending = []byte("\n")
		}
		out = append(out, []byte("data: ")...)
		out = append(out, data...)
		out = append(out, ending...)
	}
	return out
}

type droidPartialToolName struct {
	name, path string
	first      *droidToolSSEFrame
}

// Pull-based, bounded event buffering. Complete events flush immediately;
// only Chat name fragments wait until the first tool name can be restored.
// Keeping the first frame avoids starting an Anthropic tool block without a
// name when a downstream Chat-to-Messages converter consumes the stream.
type droidToolNameSSEReader struct {
	source   io.ReadCloser
	reader   *bufio.Reader
	names    *droidToolNames
	queue    []*droidToolSSEFrame
	pending  map[string]*droidPartialToolName
	heldSize int
	ready    []byte
	terminal error
}

func newDroidToolNameSSEReader(source io.ReadCloser, names *droidToolNames) io.ReadCloser {
	return &droidToolNameSSEReader{source: source, reader: bufio.NewReaderSize(source, 4096), names: names, pending: map[string]*droidPartialToolName{}}
}

func (r *droidToolNameSSEReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.ready) == 0 {
		if r.terminal != nil {
			return 0, r.terminal
		}
		frame, err := r.nextFrame()
		if err != nil {
			r.terminal = safeDroidError(err)
		}
		if frame != nil {
			r.queue = append(r.queue, frame)
			r.heldSize += len(frame.raw)
			if r.heldSize > droidErrorJSONMaxBytes {
				r.terminal = errors.New("factory tool stream buffer exceeded size limit")
				r.queue = nil
				return 0, r.terminal
			}
			if err := r.collectChatNameFragments(frame); err != nil {
				r.terminal, r.queue = err, nil
				return 0, err
			}
		}
		if len(r.pending) == 0 {
			for _, item := range r.queue {
				r.ready = append(r.ready, item.render(r.names)...)
			}
			r.queue, r.heldSize = nil, 0
		} else if r.terminal != nil {
			// Do not leak a partial alias or invent a terminal event at EOF.
			if errors.Is(r.terminal, io.EOF) {
				r.terminal = io.ErrUnexpectedEOF
			}
			r.queue = nil
		}
	}
	n := copy(p, r.ready)
	r.ready = r.ready[n:]
	return n, nil
}

func (r *droidToolNameSSEReader) nextFrame() (*droidToolSSEFrame, error) {
	frame := &droidToolSSEFrame{}
	var line []byte
	sawData := false
	for {
		part, err := r.reader.ReadSlice('\n')
		if len(frame.raw)+len(line)+len(part) > droidErrorJSONMaxBytes {
			return nil, errors.New("factory tool stream frame exceeded size limit")
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		frame.raw = append(frame.raw, line...)
		content := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if bytes.HasPrefix(content, []byte("event:")) {
			frame.event = strings.TrimSpace(string(content[6:]))
		}
		if bytes.HasPrefix(content, []byte("data:")) {
			data := bytes.TrimPrefix(content[5:], []byte(" "))
			if sawData {
				frame.data = append(frame.data, '\n')
			}
			frame.data = append(frame.data, data...)
			sawData = true
		}
		if err != nil || len(content) == 0 {
			if len(frame.raw) == 0 {
				return nil, err
			}
			frame.original = frame.data
			return frame, err
		}
		line = nil
	}
}

func (r *droidToolNameSSEReader) aliasPrefix(name string) bool {
	for alias := range r.names.reverse {
		if strings.HasPrefix(alias, name) {
			return true
		}
	}
	return false
}

func (r *droidToolNameSSEReader) finishName(key string, final bool) error {
	part := r.pending[key]
	if part == nil {
		return nil
	}
	name, aliased := r.names.reverse[part.name]
	if !aliased {
		if !final && r.aliasPrefix(part.name) {
			return nil
		}
		if final && r.aliasPrefix(part.name) && !r.names.literal[part.name] {
			return errors.New("factory returned an incomplete tool name")
		}
		name = part.name
	}
	if err := part.first.setName(part.path, name); err != nil {
		return err
	}
	delete(r.pending, key)
	return nil
}

func (r *droidToolNameSSEReader) collectChatNameFragments(frame *droidToolSSEFrame) error {
	if droidSSEEventIsError(frame.event) || droidSSEEventIsError(gjson.GetBytes(frame.data, "type").String()) || gjson.GetBytes(frame.data, "error").IsObject() {
		// An upstream error must not be hidden behind an unfinished tool name.
		r.pending = map[string]*droidPartialToolName{}
		r.queue, r.heldSize = []*droidToolSSEFrame{frame}, len(frame.raw)
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(frame.data), []byte("[DONE]")) {
		for key := range r.pending {
			if err := r.finishName(key, true); err != nil {
				return err
			}
		}
		return nil
	}
	if !gjson.ValidBytes(frame.data) {
		return nil
	}
	for ci, choice := range gjson.GetBytes(frame.data, "choices").Array() {
		choiceIndex := choice.Get("index").Int()
		if !choice.Get("index").Exists() {
			choiceIndex = int64(ci)
		}
		for ti, call := range choice.Get("delta.tool_calls").Array() {
			index := call.Get("index").Int()
			if !call.Get("index").Exists() {
				index = int64(ti)
			}
			key := fmt.Sprintf("%d/%d", choiceIndex, index)
			path := fmt.Sprintf("choices.%d.delta.tool_calls.%d.function.name", ci, ti)
			if err := r.collectName(frame, key, path, call.Get("function")); err != nil {
				return err
			}
		}
		if fn := choice.Get("delta.function_call"); fn.Exists() {
			if err := r.collectName(frame, fmt.Sprintf("%d/legacy", choiceIndex), fmt.Sprintf("choices.%d.delta.function_call.name", ci), fn); err != nil {
				return err
			}
		}
		if choice.Get("finish_reason").String() != "" {
			for key := range r.pending {
				if strings.HasPrefix(key, fmt.Sprintf("%d/", choiceIndex)) {
					if err := r.finishName(key, true); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (r *droidToolNameSSEReader) collectName(frame *droidToolSSEFrame, key, path string, fn gjson.Result) error {
	fragment := fn.Get("name").String()
	part := r.pending[key]
	if part == nil {
		if fragment == "" || !r.aliasPrefix(fragment) || r.names.reverse[fragment] != "" {
			return nil
		}
		part = &droidPartialToolName{name: fragment, path: path, first: frame}
		r.pending[key] = part
	} else if fragment != "" {
		part.name += fragment
		if err := frame.setName(path, ""); err != nil {
			return err
		}
	}
	return r.finishName(key, fn.Get("arguments").String() != "")
}

func (r *droidToolNameSSEReader) Close() error { return safeDroidError(r.source.Close()) }
