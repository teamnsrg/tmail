package rest

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/codegangsta/negroni"
	"github.com/julienschmidt/httprouter"
	"github.com/nbio/httpcontext"

	"github.com/teamnsrg/tmail/core"
)

const (
	// Max size of the posted body
	body_read_limit = 1048576
)

// LaunchServer launches HTTP server
func LaunchServer() {
	router := httprouter.New()
	router.HandlerFunc("GET", "/ping", func(w http.ResponseWriter, req *http.Request) {
		httpWriteJson(w, []byte(`{"msg": "pong"}`))
	})

	// Users handlers
	addUsersHandlers(router)
	// Queue
	addQueueHandlers(router)

	// Microservice data handler
	router.Handler("GET", "/msdata/:id", http.StripPrefix("/msdata/", http.FileServer(http.Dir(core.Cfg.GetTempDir()))))

	// Server
	n := negroni.New(negroni.NewRecovery(), NewLogger())
	n.UseHandler(router)
	addr := fmt.Sprintf("%s:%d", core.Cfg.GetRestServerIp(), core.Cfg.GetRestServerPort())

	// TLS
	if core.Cfg.GetRestServerIsTls() {
		core.Logger.Info("httpd " + addr + " TLS launched")
		log.Fatalln(http.ListenAndServeTLS(addr, path.Join(getBasePath(), "ssl/web_server.crt"), path.Join(getBasePath(), "ssl/web_server.key"), n))
	} else {
		core.Logger.Info("httpd " + addr + " launched")
		log.Fatalln(http.ListenAndServe(addr, n))
	}
}

// wrapHandler puts httprouter.Params in query context
// in order to keep compatibily with http.Handler
func wrapHandler(h func(http.ResponseWriter, *http.Request)) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
		httpcontext.Set(r, "params", ps)
		h(w, r)
	}
}

// getBasePath is a helper for retrieving app path
func getBasePath() string {
	p, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	return p
}
