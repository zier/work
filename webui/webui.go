package webui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/braintree/manners"
	"github.com/garyburd/redigo/redis"
	"github.com/gocraft/web"
	"github.com/gocraft/work"
	"github.com/zier/work/webui/internal/assets"
)

// Server implements an HTTP server which exposes a JSON API to view and manage gocraft/work items.
type Server struct {
	namespace string
	pool      *redis.Pool
	client    *work.Client
	hostPort  string
	server    *manners.GracefulServer
	wg        sync.WaitGroup
	router    *web.Router
}

type Admin struct {
	Username string
	Password string
}

type context struct {
	*Server
	Admin *Admin
}

func (c *context) AdminRequired(rw web.ResponseWriter, r *web.Request, next web.NextMiddlewareFunc) {
	rw.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)

	s := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(s) != 2 {
		http.Error(rw, "Not authorized", 401)
		return
	}

	b, err := base64.StdEncoding.DecodeString(s[1])
	if err != nil {
		http.Error(rw, err.Error(), 401)
		return
	}

	pair := strings.SplitN(string(b), ":", 2)
	if len(pair) != 2 {
		http.Error(rw, "Not authorized", 401)
		return
	}

	if pair[0] != c.Admin.Username && pair[1] != c.Admin.Password {
		http.Error(rw, "Not authorized", 401)
		return
	}

	next(rw, r)
}

// NewServer creates and returns a new server. The 'namespace' param is the redis namespace to use. The hostPort param is the address to bind on to expose the API.
func NewServer(namespace string, pool *redis.Pool, hostPort, username, password string) *Server {
	c := context{
		Admin: &Admin{
			Username: username,
			Password: password,
		},
	}
	router := web.New(c)
	router.Middleware(c.AdminRequired)
	server := &Server{
		namespace: namespace,
		pool:      pool,
		client:    work.NewClient(namespace, pool),
		hostPort:  hostPort,
		server:    manners.NewWithServer(&http.Server{Addr: hostPort, Handler: router}),
		router:    router,
	}

	router.Middleware(func(c *context, rw web.ResponseWriter, r *web.Request, next web.NextMiddlewareFunc) {
		c.Server = server
		next(rw, r)
	})
	router.Middleware(func(rw web.ResponseWriter, r *web.Request, next web.NextMiddlewareFunc) {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		next(rw, r)
	})
	router.Get("/queues", c.queues)
	router.Get("/worker_pools", c.workerPools)
	router.Get("/busy_workers", c.busyWorkers)
	router.Get("/retry_jobs", c.retryJobs)
	router.Get("/scheduled_jobs", c.scheduledJobs)
	router.Get("/dead_jobs", c.deadJobs)
	router.Post("/delete_dead_job/:died_at:\\d.*/:job_id", c.deleteDeadJob)
	router.Post("/retry_dead_job/:died_at:\\d.*/:job_id", c.retryDeadJob)
	router.Post("/delete_all_dead_jobs", c.deleteAllDeadJobs)
	router.Post("/retry_all_dead_jobs", c.retryAllDeadJobs)

	//
	// Build the HTML page:
	//
	assetRouter := router.Subrouter(context{}, "")
	assetRouter.Get("/", func(c *context, rw web.ResponseWriter, req *web.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Write(assets.MustAsset("index.html"))
	})
	assetRouter.Get("/work.js", func(c *context, rw web.ResponseWriter, req *web.Request) {
		rw.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		rw.Write(assets.MustAsset("work.js"))
	})

	return server
}

// Start starts the server listening for requests on the hostPort specified in NewServer.
func (w *Server) Start() {
	w.wg.Add(1)
	go func(w *Server) {
		w.server.ListenAndServe()
		w.wg.Done()
	}(w)
}

// Stop stops the server and blocks until it has finished.
func (w *Server) Stop() {
	w.server.Close()
	w.wg.Wait()
}

func (c *context) queues(rw web.ResponseWriter, r *web.Request) {
	response, err := c.client.Queues()
	render(rw, response, err)
}

func (c *context) workerPools(rw web.ResponseWriter, r *web.Request) {
	response, err := c.client.WorkerPoolHeartbeats()
	render(rw, response, err)
}

func (c *context) busyWorkers(rw web.ResponseWriter, r *web.Request) {
	observations, err := c.client.WorkerObservations()
	if err != nil {
		renderError(rw, err)
		return
	}

	var busyObservations []*work.WorkerObservation
	for _, ob := range observations {
		if ob.IsBusy {
			busyObservations = append(busyObservations, ob)
		}
	}

	render(rw, busyObservations, err)
}

func (c *context) retryJobs(rw web.ResponseWriter, r *web.Request) {
	page, err := parsePage(r)
	if err != nil {
		renderError(rw, err)
		return
	}

	jobs, count, err := c.client.RetryJobs(page)
	if err != nil {
		renderError(rw, err)
		return
	}

	response := struct {
		Count int64            `json:"count"`
		Jobs  []*work.RetryJob `json:"jobs"`
	}{Count: count, Jobs: jobs}

	render(rw, response, err)
}

func (c *context) scheduledJobs(rw web.ResponseWriter, r *web.Request) {
	page, err := parsePage(r)
	if err != nil {
		renderError(rw, err)
		return
	}

	jobs, count, err := c.client.ScheduledJobs(page)
	if err != nil {
		renderError(rw, err)
		return
	}

	response := struct {
		Count int64                `json:"count"`
		Jobs  []*work.ScheduledJob `json:"jobs"`
	}{Count: count, Jobs: jobs}

	render(rw, response, err)
}

func (c *context) deadJobs(rw web.ResponseWriter, r *web.Request) {
	page, err := parsePage(r)
	if err != nil {
		renderError(rw, err)
		return
	}

	jobs, count, err := c.client.DeadJobs(page)
	if err != nil {
		renderError(rw, err)
		return
	}

	response := struct {
		Count int64           `json:"count"`
		Jobs  []*work.DeadJob `json:"jobs"`
	}{Count: count, Jobs: jobs}

	render(rw, response, err)
}

func (c *context) deleteDeadJob(rw web.ResponseWriter, r *web.Request) {
	diedAt, err := strconv.ParseInt(r.PathParams["died_at"], 10, 64)
	if err != nil {
		renderError(rw, err)
		return
	}

	err = c.client.DeleteDeadJob(diedAt, r.PathParams["job_id"])

	render(rw, map[string]string{"status": "ok"}, err)
}

func (c *context) retryDeadJob(rw web.ResponseWriter, r *web.Request) {
	diedAt, err := strconv.ParseInt(r.PathParams["died_at"], 10, 64)
	if err != nil {
		renderError(rw, err)
		return
	}

	err = c.client.RetryDeadJob(diedAt, r.PathParams["job_id"])

	render(rw, map[string]string{"status": "ok"}, err)
}

func (c *context) deleteAllDeadJobs(rw web.ResponseWriter, r *web.Request) {
	err := c.client.DeleteAllDeadJobs()
	render(rw, map[string]string{"status": "ok"}, err)
}

func (c *context) retryAllDeadJobs(rw web.ResponseWriter, r *web.Request) {
	err := c.client.RetryAllDeadJobs()
	render(rw, map[string]string{"status": "ok"}, err)
}

func render(rw web.ResponseWriter, jsonable interface{}, err error) {
	if err != nil {
		renderError(rw, err)
		return
	}

	jsonData, err := json.MarshalIndent(jsonable, "", "\t")
	if err != nil {
		renderError(rw, err)
		return
	}
	rw.Write(jsonData)
}

func renderError(rw http.ResponseWriter, err error) {
	rw.WriteHeader(500)
	fmt.Fprintf(rw, `{"error": "%s"}`, err.Error())
}

func parsePage(r *web.Request) (uint, error) {
	err := r.ParseForm()
	if err != nil {
		return 0, err
	}

	pageStr := r.Form.Get("page")
	if pageStr == "" {
		pageStr = "1"
	}

	page, err := strconv.ParseUint(pageStr, 10, 0)
	return uint(page), err
}
