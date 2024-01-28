package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type App struct {
	name        string
	mu          sync.Mutex
	jobServers  int
	webServers  int
	baseWebPort int
	webServerIp string
	dockerImage string
}

type Deploy struct {
	w        http.ResponseWriter
	duration time.Duration
	app      *App
}

type Derailleur struct {
	apps map[string]*App
}

func main() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)

	listenOn := "100.67.131.62:8050"
	d := Derailleur{
		apps: make(map[string]*App),
	}
	d.apps["bike-app"] = &App{
		name:        "bike-app",
		jobServers:  3,
		webServers:  3,
		baseWebPort: 8090,
		webServerIp: "100.67.131.62",
		dockerImage: "ghcr.io/eljojo/bike-app:main",
	}
	d.startServer(listenOn)
}

func (a *App) attemptDeploy(w http.ResponseWriter) (*Deploy, error) {
	if !a.mu.TryLock() {
		return nil, fmt.Errorf("another deploy is already running for %s", a.name)
	}
	defer a.mu.Unlock()

	deploy := Deploy{
		w:   w,
		app: a,
	}
	return &deploy, nil // Return the deploy instance
}

func (d *Deploy) perform() error {
	start := time.Now()
	d.log("🧑‍💻🧿 deploying " + d.app.name)

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
	err = d.postDeploy()
	if err != nil {
		return fmt.Errorf("failed to run post-deploy tasks: %w", err)
	}

	d.log("🎉 great success, app is running new version")

	d.duration = time.Since(start)
	return nil
}

func (d *Deploy) pullDockerImage() error {
	d.log("🐳⤵️  pulling docker image")
	cmd, err := exec.Command("/run/current-system/sw/bin/docker", "pull", d.app.dockerImage).CombinedOutput()
	d.log(string(cmd))
	return err
}

func (d *Deploy) restartJobs() error {
	d.log("👔 restarting jobs")
	for i := 1; i <= d.app.jobServers; i++ {
		d.log(fmt.Sprintf("restarting job server %d", i))
		cmd, err := exec.Command("/run/current-system/sw/bin/systemctl", "restart", fmt.Sprintf("docker-%s-jobs-%d", d.app.name, i)).CombinedOutput()
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
	for i := 1; i <= d.app.webServers; i++ {
		d.log(fmt.Sprintf("restarting web server %d", i))
		cmd, err := exec.Command("/run/current-system/sw/bin/systemctl", "restart", fmt.Sprintf("docker-%s-web-%d", d.app.name, i)).CombinedOutput()
		if err != nil {
			d.log(string(cmd))
			return err
		}
		err = d.waitForWebServer(d.app.baseWebPort + i)
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
	container_name := fmt.Sprintf("%s-jobs-1", d.app.name) // Adjusted to dynamic container name
	log.Debugf("running rake task %s on %s ", name, container_name)
	cmd, err := exec.Command(
		"/run/current-system/sw/bin/docker", "exec", "-i", "-e", "NEW_RELIC_AGENT_ENABLED=false", container_name, "/app/bin/rake", name,
	).CombinedOutput()
	d.log(string(cmd))
	return err
}

func (d *Deploy) waitForWebServer(serverPort int) error {
	url := fmt.Sprintf("http://%s:%d/_ping", d.app.webServerIp, serverPort)
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
	fmt.Fprintf(d.w, msg+"\n")
	if f, ok := d.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (d *Derailleur) handleDeployRequest(w http.ResponseWriter, req *http.Request) {
	appName := strings.TrimPrefix(req.URL.Path, "/")
	appName = strings.TrimSuffix(appName, "/deploy")

	if req.Method != "POST" {
		http.Error(w, "404 not found.", http.StatusNotFound)
		return
	}

	user, pass, ok := req.BasicAuth()
	if !ok || user != os.Getenv("AUTH_USER") || pass != os.Getenv("AUTH_PASSWORD") {
		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
		http.Error(w, "Unauthorized.", http.StatusUnauthorized)
		log.WithFields(log.Fields{"IP": req.RemoteAddr}).Warn("an unauthorized deploy was attempted")
		return
	}

	if app, ok := d.apps[appName]; ok {
		log.WithFields(log.Fields{"IP": req.RemoteAddr}).Info("attempting deploy")
		w.WriteHeader(http.StatusOK) // all responses are 200, even failed deploys :(

		deploy, err := app.attemptDeploy(w)
		if err != nil {
			log.Error("🚨 Deploy failed! ", err)
			fmt.Fprintf(w, "🚨 deploy failed for %s: %v\n", app.name, err)
		} else {
			fmt.Fprintf(w, "deploy finished for %s! it took %v seconds\n", app.name, deploy.duration)
		}
	} else {
		http.Error(w, "App not found.", http.StatusNotFound)
	}
}

func (d *Derailleur) handleStatusRequest(w http.ResponseWriter, req *http.Request) {
	appName := strings.TrimPrefix(req.URL.Path, "/")
	appName = strings.TrimSuffix(appName, "/")
	if app, ok := d.apps[appName]; ok {
		log.WithFields(log.Fields{"IP": req.RemoteAddr, "App": appName}).Info("status request")
		w.WriteHeader(http.StatusOK)
		if app.isDeploying() {
			fmt.Fprintf(w, "%s is currently deploying", appName)
		} else {
			fmt.Fprintf(w, "%s is not deploying right now", appName)
		}
	} else {
		http.Error(w, "App not found.", http.StatusNotFound)
	}
}

func (d *Derailleur) handleRootRequest(w http.ResponseWriter, req *http.Request) {
	log.WithFields(log.Fields{"IP": req.RemoteAddr}).Info("root request")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "Apps and their deployment status:")
	for name, app := range d.apps {
		status := "not deploying"
		if app.isDeploying() {
			status = "deploying"
		}
		fmt.Fprintf(w, "- %s: %s\n", name, status)
	}
}

func (a *App) isDeploying() bool {
	if a.mu.TryLock() {
		a.mu.Unlock()
		return false
	}
	return true
}

func (d *Derailleur) startServer(listenOn string) {
	fmt.Println("listening on", listenOn)
	http.HandleFunc("/", d.handleRootRequest)
	for appName := range d.apps {
		http.HandleFunc(fmt.Sprintf("/%s/", appName), d.handleStatusRequest)
		http.HandleFunc(fmt.Sprintf("/%s/deploy", appName), d.handleDeployRequest)
	}
	http.ListenAndServe(listenOn, nil)
}
