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
	duration    time.Duration
	logString   string
	jobServers  int
	webServers  int
	baseWebPort int
	webServerIp string
	dockerImage string
}

func main() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)

	listenOn := ":8050"
	app := Derailleur{}
	app.startServer(listenOn)
}

func (a *Derailleur) attemptDeploy() (d Deploy, e error) {
	if !a.mu.TryLock() {
		err := fmt.Errorf("another deploy is already running")
		return Deploy{}, err
	}
	defer a.mu.Unlock()

	deploy := Deploy{
		jobServers:  2,
		webServers:  3,
		baseWebPort: 8090,
		webServerIp: "100.67.131.62",
		dockerImage: "ghcr.io/eljojo/bike-app:main",
	}
	err := deploy.start()
	return deploy, err
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

func (d *Deploy) pullDockerImage() error {
	d.log("🐳⤵️ pulling docker image")
	cmd, err := exec.Command("docker", "pull", d.dockerImage).CombinedOutput()
	d.log(string(cmd))
	return err
}

func (d *Deploy) restartJobs() error {
	d.log("👔 restarting jobs")
	for i := 1; i <= d.jobServers; i++ {
		d.log(fmt.Sprintf("restarting job server %d", i))
		cmd, err := exec.Command("systemctl", "restart", fmt.Sprintf("docker-bike-app-jobs-%d", i)).CombinedOutput()
		if err != nil {
			d.log(string(cmd))
			return err
		}
		time.Sleep(5 * time.Second)
	}

	return nil
}

func (d *Deploy) releaseApp() error {
	d.log("💽🧺 running migrations and uploading assets to CDN")
	return d.runRakeTask("fly:release")
}

func (d *Deploy) restartWeb() error {
	d.log("🌐 restarting web servers")
	for i := 1; i <= d.webServers; i++ {
		d.log(fmt.Sprintf("restarting web server %d", i))
		cmd, err := exec.Command("systemctl", "restart", fmt.Sprintf("docker-bike-app-web-%d", i)).CombinedOutput()
		if err != nil {
			d.log(string(cmd))
			return err
		}
		err = d.waitForWebServer(d.baseWebPort + i)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Deploy) postDeploy() error {
	d.log("🧹 running post-deploy cleanup")
	return d.runRakeTask("fly:clear_cdn")
}

func (d *Deploy) runRakeTask(name string) error {
	container_name := "bike-app-jobs-1" // TODO: make this a new task-runner container
	log.Debugf("running rake task %s on %s ", name, container_name)
	cmd, err := exec.Command(
		"docker", "exec", "-i", "-e", "NEW_RELIC_AGENT_ENABLED=false", container_name, "/app/bin/rake", name,
	).CombinedOutput()
	d.log(string(cmd))
	return err
}

func (d *Deploy) waitForWebServer(serverPort int) error {
	url := fmt.Sprintf("http://%s:%d/_ping", d.webServerIp, serverPort)
	log.Debug("checking ", url)
	timeoutChan := time.After(60 * time.Second)
	for {
		select {
		case <-timeoutChan:
			return fmt.Errorf("timed out waiting for server to start")
		default:
			_, err := http.Get(url)
			if err == nil {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (d *Deploy) log(msg string) {
	log.Info(msg)
	d.logString = d.logString + msg + "\n"
}

func (a *Derailleur) handleDeployRequest(w http.ResponseWriter, req *http.Request) {
	user, pass, ok := req.BasicAuth()
	if !ok || user != os.Getenv("AUTH_USER") || pass != os.Getenv("AUTH_PASSWORD") {
		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
		http.Error(w, "Unauthorized.", http.StatusUnauthorized)
		log.WithFields(log.Fields{"IP": req.RemoteAddr}).Warn("an unauthorized deploy was attempted")
		return
	}

	log.WithFields(log.Fields{"IP": req.RemoteAddr}).Info("attempting deploy")

	deploy, err := a.attemptDeploy()
	if err != nil {
		log.Error("🚨 Deploy failed! ", err)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "🚨 deploy failed: %v\n", err)
	} else {
		fmt.Fprintf(w, "deploy complete, it took %v seconds\n", deploy.duration)
	}
	fmt.Fprintf(w, deploy.logString)
}

func (a *Derailleur) isDeploying() bool {
	if a.mu.TryLock() {
		a.mu.Unlock()
		return false
	}

	return true
}

func (a *Derailleur) handleStatusRequest(w http.ResponseWriter, req *http.Request) {
	log.WithFields(log.Fields{"IP": req.RemoteAddr}).Info("status request")

	if a.isDeploying() {
		fmt.Fprintf(w, "deploy in progress")
	} else {
		fmt.Fprintf(w, "nothing is deploying right now")
	}
}

func (a *Derailleur) startServer(listenOn string) {
	fmt.Println("listening on ", listenOn)
	http.HandleFunc("/", a.handleStatusRequest)
	http.HandleFunc("/deploy", a.handleDeployRequest)
	http.ListenAndServe(listenOn, nil)
}
