package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	dir := os.Getenv("JHV_FRONTEND_SPA_DIR")
	if dir == "" {
		dir = "web/dist"
	}
	handler, err := newGateway(os.Getenv("JHV_FRONTEND_BACKEND_URL"), os.Getenv("JHV_FRONTEND_BACKEND_CA_FILE"), dir)
	if err != nil {
		log.Fatal(err)
	}

	addr := os.Getenv("JHV_FRONTEND_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	cert, key := os.Getenv("JHV_FRONTEND_TLS_CERT_FILE"), os.Getenv("JHV_FRONTEND_TLS_KEY_FILE")
	if (cert == "") != (key == "") {
		log.Fatal("для HTTPS фронтенда задайте JHV_FRONTEND_TLS_CERT_FILE и JHV_FRONTEND_TLS_KEY_FILE")
	}
	server := &http.Server{
		Addr: addr, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	log.Printf("фронтенд слушает %s", addr)
	if cert != "" {
		err = server.ListenAndServeTLS(cert, key)
	} else {
		err = server.ListenAndServe()
	}
	if !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
