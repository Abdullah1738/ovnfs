package admin

import (
	"context"
	"encoding/json"
	"net"
)

// Loading describes registry records restored before the daemon is ready.
type Loading struct {
	PID       int `json:"pid"`
	Completed int `json:"completed"`
	Total     int `json:"total"`
}

// Loading publishes progress independently of the registry being restored.
func (s *Server) Loading(pid, completed, total int) {
	s.progressMu.Lock()
	s.progress = Loading{PID: pid, Completed: completed, Total: total}
	s.progressMu.Unlock()
}

// Probe answers during startup without granting access to partially loaded state.
// Older servers may not support it; callers must retain the regular hello path.
func Probe(ctx context.Context, path string) (Loading, bool, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return Loading{}, false, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := writeFrame(conn, map[string]any{"probe": struct{}{}}); err != nil {
		return Loading{}, false, err
	}
	obj, err := readFrame(conn)
	if err != nil {
		return Loading{}, false, err
	}
	var p Loading
	var ready bool
	if err = json.Unmarshal(obj["loading"], &p); err == nil {
		err = json.Unmarshal(obj["ready"], &ready)
	}
	return p, ready, err
}
