package main

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Derailleur struct {
	mu sync.Mutex
}

type Deploy struct {
	response http.ResponseWriter
}

func (d *Deploy) start() {
	log.Info("deploying...")
	time.Sleep(10 * time.Second)
	fmt.Fprintf(d.response, "hello world")
	log.Info("deploy completed!")
}

func (a *Derailleur) hello(w http.ResponseWriter, req *http.Request) {
	log.WithFields(log.Fields{
		"IP": req.RemoteAddr,
	}).Info("new deploy request")

	if a.mu.TryLock() == false {
		log.Warn("ERROR: deploy already running, cancelling...")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("already deploying! please try again later."))
		return
	}
	defer a.mu.Unlock()

	deploy := Deploy{response: w}
	deploy.start()
}

func (a *Derailleur) startServer(listenOn string) {
	fmt.Println("listening on ", listenOn)
	http.HandleFunc("/", a.hello)
	http.ListenAndServe(listenOn, nil)
}

func main() {
	listenOn := ":8090"
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
	app := Derailleur{}
	app.startServer(listenOn)
}
