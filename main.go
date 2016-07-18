package main

import (
	"container/ring"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/newrelic/sidecar/catalog"
	"gopkg.in/alecthomas/kingpin.v1"
)

const (
	INITIAL_RING_SIZE = 20
	BUFFER_SIZE       = 25
)

var (
	changes        *ring.Ring
	changesChan    chan catalog.StateChangedEvent
	ringSize       int
	listeners      []chan Notification
	listenLock     sync.Mutex
)

type Notification struct {
	Event       *catalog.ChangeEvent
	ClusterName string
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
}

type CliOpts struct {
	ConfigFile *string
}

type ApiErrors struct {
	Errors []string
}

type ApiMessage struct {
	Message string
}

type ApiStatus struct {
	Message     string
	LastChanged time.Time
}

func NotificationFromEvent(evt *catalog.StateChangedEvent) *Notification {
	return &Notification{
		Event: &evt.ChangeEvent,
		ClusterName: evt.State.ClusterName,
	}
}

func exitWithError(err error, message string) {
	if err != nil {
		log.Fatal("%s: %s", message, err.Error())
	}
}

func parseCommandLine() *CliOpts {
	var opts CliOpts
	opts.ConfigFile = kingpin.Flag("config-file", "The config file to use").Short('f').Default("superside.toml").String()
	kingpin.Parse()
	return &opts
}

// The health check endpoint. Tells us if HAproxy is running and has
// been properly configured. Since this is critical infrastructure this
// helps make sure a host is not "down" by havign the proxy down.
func healthHandler(response http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	response.Header().Set("Content-Type", "application/json")

	//errors := make([]string, 0)

	message, _ := json.Marshal(ApiStatus{
		Message: "Healthy!",
	})

	response.Write(message)
}

// Returns the currently stored state as a JSON blob
func stateHandler(response http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	response.Header().Set("Content-Type", "application/json")

	var changeHistory []Notification
	changes.Do(func(evt interface{}) {
		if evt != nil {
			event := evt.(catalog.StateChangedEvent)
			changeHistory = append(changeHistory, *NotificationFromEvent(&event))
		}
	})

	message, _ := json.Marshal(changeHistory)
	response.Write(message)
}

// Receives POSTed state updates from Sidecar instances
func updateHandler(response http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	response.Header().Set("Content-Type", "application/json")

	data, err := ioutil.ReadAll(req.Body)
	if err != nil {
		message, _ := json.Marshal(ApiErrors{[]string{err.Error()}})
		response.WriteHeader(http.StatusInternalServerError)
		response.Write(message)
		return
	}

	var evt catalog.StateChangedEvent
	err = json.Unmarshal(data, &evt)
	if err != nil {
		response.WriteHeader(http.StatusInternalServerError)
		return
	}

	changesChan <- evt // Rely on channel buffer. We block if channel is full

	message, _ := json.Marshal(ApiMessage{Message: "OK"})
	response.Write(message)
}

// Handle the listening endpoint websocket
func websockHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error(err)
		return
	}

	listenChan := getListener()
	defer close(listenChan)

	for {
		evt := <-listenChan

		message, err := json.Marshal(evt)
		if err != nil {
			log.Error("Error marshaling JSON event " + err.Error())
			continue
		}

		if err = conn.WriteMessage(websocket.TextMessage, message); err != nil {
			log.Warn(err.Error())
			return
		}
	}
}

// Subscribe a listener
func getListener() chan Notification {
	listenChan := make(chan Notification, 100)
	listenLock.Lock()
	listeners = append(listeners, listenChan)
	listenLock.Unlock()

	return listenChan
}

// Announce changes to all listeners
func tellListeners(evt *catalog.StateChangedEvent) {
	listenLock.Lock()
	defer listenLock.Unlock()

	// Try to tell the listener about the change but use a select
	// to protect us from any blocking readers.
	for _, listener := range listeners {
		select {
		case listener <- *NotificationFromEvent(evt):
		default:
		}
	}
}

// Start the HTTP server and begin handling requests. This is a
// blocking call.
func serveHttp(listenIp string, listenPort int) {
	listenStr := fmt.Sprintf("%s:%d", listenIp, listenPort)

	log.Infof("Starting up on %s", listenStr)
	fs := http.FileServer(http.Dir("public"))
	router := mux.NewRouter()

	router.HandleFunc("/update", updateHandler).Methods("POST")
	router.HandleFunc("/health", healthHandler).Methods("GET")
	router.HandleFunc("/state", stateHandler).Methods("GET")
	router.HandleFunc("/listen", websockHandler).Methods("GET")
	router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", fs))
	http.Handle("/", handlers.LoggingHandler(os.Stdout, router))

	err := http.ListenAndServe(listenStr, nil)
	if err != nil {
		log.Fatalf("Can't start http server: %s", err.Error())
	}
}

// Linearize the updates coming in from the async HTTP handler
func processUpdates() {
	for evt := range changesChan {
		newEntry := &ring.Ring{Value: evt}

		if ringSize == 0 {
			changes = newEntry
			ringSize += 1
		} else if ringSize < INITIAL_RING_SIZE {
			changes.Prev().Link(newEntry)
			ringSize += 1
		} else {
			changes = changes.Prev()
			changes.Unlink(1)
			changes = changes.Next()
			changes.Prev().Link(newEntry)
		}

		tellListeners(&evt)
	}
}

func main() {
	opts := parseCommandLine()
	config := parseConfig(*opts.ConfigFile)

	changesChan = make(chan catalog.StateChangedEvent, BUFFER_SIZE)

	go processUpdates()

	serveHttp(config.Superside.BindIP, config.Superside.BindPort)
}
