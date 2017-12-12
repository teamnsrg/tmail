package rest

import (
	"net/http"

	"github.com/teamnsrg/tmail/core"
)

// httpWriteJson send a json response
func httpWriteJson(w http.ResponseWriter, out []byte) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Write(out)
}

// httpErrorJson send and json formated error
func httpWriteErrorJson(w http.ResponseWriter, httpStatus int, msg, raw string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(httpStatus)
	w.Write([]byte(`{"msg":"` + msg + `","raw":"` + raw + `"}`))
}

// httpGetScheme returns http ou https
func httpGetScheme() string {
	scheme := "http"
	if core.Cfg.GetRestServerIsTls() {
		scheme = "https"
	}
	return scheme
}
