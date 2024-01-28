package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type App struct {
	name          string
	mu            sync.Mutex
	jobServers    int
	webServers    int
	baseWebPort   int
	webServerIp   string
	containerRepo string
}

type Deploy struct {
	w        http.ResponseWriter
	duration time.Duration
	app      *App
	tag      string
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
		name:          "bike-app",
		jobServers:    3,
		webServers:    3,
		baseWebPort:   8090,
		webServerIp:   "100.67.131.62",
		containerRepo: "ghcr.io/eljojo/bike-app",
	}
	d.apps["bike-place-staging"] = &App{
		name:          "bike-place-staging",
		jobServers:    1,
		webServers:    2,
		baseWebPort:   8060,
		webServerIp:   "100.67.131.62",
		containerRepo: "ghcr.io/eljojo/bike-place",
	}
	d.startServer(listenOn)
}

func (a *App) attemptDeploy(w http.ResponseWriter, tag string) (*Deploy, error) {
	if !a.mu.TryLock() {
		return nil, fmt.Errorf("another deploy is already running for %s", a.name)
	}
	defer a.mu.Unlock()

	deploy := Deploy{
		w:   w,
		app: a,
		tag: tag,
	}
	err := deploy.perform()
	return &deploy, err // Return the deploy instance
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
	d.log("🐳⤵️  pulling docker image with tag: " + d.tag)

	dockerImage := d.app.containerRepo + ":" + d.tag
	newImageTag := fmt.Sprintf("%s:release", d.app.name)

	// Pull the specific tag
	cmd, err := exec.Command("/run/current-system/sw/bin/docker", "pull", dockerImage).CombinedOutput()
	if err != nil {
		d.log(string(cmd))
		return err
	}

	// Re-tag the image to app-name:release
	cmd, err = exec.Command("/run/current-system/sw/bin/docker", "tag", dockerImage, newImageTag).CombinedOutput()
	d.log(string(cmd))
	if err != nil {
		return err
	}

	return nil
}

func (d *Deploy) restartJobs() error {
	d.log("👔 restarting jobs")
	for i := 1; i <= d.app.jobServers; i++ {
		jobServerName := fmt.Sprintf("%s-jobs-%d", d.app.name, i)
		d.log(fmt.Sprintf("restarting job server %s", jobServerName))
		cmd, err := exec.Command("/run/current-system/sw/bin/systemctl", "restart", fmt.Sprintf("docker-%s", jobServerName)).CombinedOutput()
		if err != nil {
			d.log(string(cmd))
			return err
		}

		// Wait for the container to be up and running
		if err := d.waitForContainer(jobServerName, 60*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deploy) releaseApp() error {
	d.log("💽🧺 running migrations and uploading assets to CDN")
	return d.runRakeTask("infra:pre_deploy")
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
	return d.runRakeTask("infra:post_deploy")
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

func (d *Deploy) waitForContainer(containerName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for container %s to start", containerName)
		}

		cmd := exec.Command("/run/current-system/sw/bin/sh", "-c", fmt.Sprintf("/run/current-system/sw/bin/docker ps | grep %s", containerName))
		output, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(string(output), containerName) {
			return nil // Container is running
		}
		d.log(string(output))
		d.log(fmt.Sprintf("%v", err))

		time.Sleep(1 * time.Second)
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

	tag := req.URL.Query().Get("tag") // Get the tag from the query parameter
	if tag == "" || !isValidInput(tag) {
		http.Error(w, "Missing tag parameter.", http.StatusBadRequest)
		return
	}

	if app, ok := d.apps[appName]; ok {
		log.WithFields(log.Fields{"IP": req.RemoteAddr}).Info("attempting deploy")
		w.WriteHeader(http.StatusOK) // all responses are 200, even failed deploys :(

		deploy, err := app.attemptDeploy(w, tag)
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

func isValidInput(input string) bool {
	validInputRegex := regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
	return validInputRegex.MatchString(input)
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
