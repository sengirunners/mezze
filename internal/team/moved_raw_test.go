package team_test

import (
	"bytes"
	"github.com/sengirunners/potluck/kit/api"
	"net/http"
	"net/http/httptest"
)

type rawRes struct {
	code int
	body string
	hdr  http.Header
}

func raw(d *daemon, project, method, path, body string) rawRes {
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, rd)
	if project != "" {
		r.Header.Set(api.HeaderProject, project)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	d.dm.SocketHandler().ServeHTTP(w, r)
	return rawRes{w.Code, w.Body.String(), w.Header()}
}

func trunc(s string) string {
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}
func around(s string, needles ...string) string { return trunc(s) }
