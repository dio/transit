package bodytransform

import (
	"encoding/json"

	"github.com/dio/transit/up"
)

func init() {
	up.RegisterWithMutableBody("body-transform", OnReq, onBody, nil)
}

// OnReq logs the method and path of every incoming request.
func OnReq(w *up.Writer, r *up.Request) {
	w.Log(up.LogInfo, "body-transform: %s %s", r.Method, r.Path)
}

func onBody(w *up.Writer, chunk *up.BodyChunk) {
	out, ok := TransformBody(chunk.Data)
	if !ok {
		return
	}
	w.SetRequestBody(out)
}

// TransformBody renames the "message" key to "text" in a JSON object.
// Returns (transformed, true) when the input is valid JSON containing a
// "message" key and a transformation was applied.
// Returns (nil, false) when the input is empty, non-JSON, or has no "message" key.
func TransformBody(data []byte) ([]byte, bool) {
	if len(data) == 0 {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	v, ok := m["message"]
	if !ok {
		return nil, false
	}
	delete(m, "message")
	m["text"] = v
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}
