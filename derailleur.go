package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Derailleur struct {
	mu sync.Mutex
}

type Deploy struct {
	duration   time.Duration
	logString  string
	jobServers int
	webServers int
}

func (d *Deploy) pullDockerImage() error {
	d.log("pulling docker image")
	cmd, err := exec.Command("docker", "pull", "ghcr.io/eljojo/bike-app:main").CombinedOutput()
	d.log(string(cmd))
	return err
}

func (d *Deploy) restartJobs() error {
	d.log("restarting jobs")
	time.Sleep(2 * time.Second)
	return nil
}

func (d *Deploy) releaseApp() error {
	d.log("💽🧺 running migrations and uploading assets to CDN")
	time.Sleep(2 * time.Second)
	return nil
}

func (d *Deploy) restartWeb() error {
	d.log("restarting web servers")
	time.Sleep(2 * time.Second)
	return nil
}

func (d *Deploy) postDeploy() error {
	d.log("running post-deploy")
	time.Sleep(2 * time.Second)
	return nil
}

func (d *Deploy) start() error {
	start := time.Now()
	d.log("🧑‍💻🧿 deploying bike-app")

	err := d.pullDockerImage()
	if err != nil {
		return fmt.Errorf("failed to pull docker image: %w", err)
	}

	err = d.restartJobs()
	if err != nil {
		return fmt.Errorf("failed to restart job servers: %w", err)
	}
	err = d.releaseApp()
	if err != nil {
		return fmt.Errorf("failed to release app: %w", err)
	}
	err = d.restartWeb()
	if err != nil {
		return fmt.Errorf("failed to restart web servers: %w", err)
	}

	d.log("🎉 great success, app is running new version")

	err = d.postDeploy()
	if err != nil {
		return fmt.Errorf("failed to run post-deploy tasks: %w", err)
	}

	d.duration = time.Since(start)
	return nil
}

func (d *Deploy) log(msg string) {
	log.Info(msg)
	d.logString = d.logString + msg + "\n"
}

func (a *Derailleur) attemptDeploy() (d Deploy, e error) {
	if a.mu.TryLock() == false {
		err := fmt.Errorf("another deploy is already running")
		return Deploy{}, err
	}
	defer a.mu.Unlock()

	deploy := Deploy{
		jobServers: 2,
		webServers: 3,
	}
	err := deploy.start()
	return deploy, err
}

func (a *Derailleur) handleDeployRequest(w http.ResponseWriter, req *http.Request) {
	log.WithFields(log.Fields{"IP": req.RemoteAddr}).Info("attempting deploy")

	deploy, err := a.attemptDeploy()
	if err != nil {
		log.Error("Deploy failed! ", err)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "deploy failed: %v\n", err)
	} else {
		fmt.Fprintf(w, "deploy complete, it took %v seconds\n", deploy.duration)
	}
	fmt.Fprintf(w, deploy.logString)
}

func (a *Derailleur) startServer(listenOn string) {
	fmt.Println("listening on ", listenOn)
	http.HandleFunc("/", a.handleDeployRequest)
	http.ListenAndServe(listenOn, nil)
}

func main() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)

	//listenOn := ":8090"
	app := Derailleur{}
	// app.startServer(listenOn)
	app.attemptDeploy()
}
