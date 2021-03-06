package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"strconv"

	"github.com/coreos/etcd/client"
	etcderr "github.com/coreos/etcd/error"
	"golang.org/x/net/context"
	"golang.org/x/net/proxy"
)

var (
	socksProxy      = os.Getenv("VCB_SOCK_PROXY")
	etcdPeers       = os.Getenv("VCB_ETCD_PEERS")
	cooldownSeconds = os.Getenv("VCB_COOLDOWN_SECONDS")

	addressRegex = regexp.MustCompile(`^[\.\-:\/\w]*:[0-9]{2,5}$`)
)

func main() {
	if etcdPeers == "" {
		etcdPeers = "http://localhost:2379"
	}

	transport := client.DefaultTransport

	if socksProxy != "" {
		dialer, _ := proxy.SOCKS5("tcp", socksProxy, nil, proxy.Direct)
		transport = &http.Transport{Dial: dialer.Dial}
	}

	peers := strings.Split(etcdPeers, ",")
	log.Printf("etcd peers are %v\n", peers)

	cfg := client.Config{
		Endpoints:               peers,
		Transport:               transport,
		HeaderTimeoutPerRequest: 5 * time.Second,
	}

	etcd, err := client.New(cfg)
	if err != nil {
		log.Fatalf("failed to start etcd client: %v\n", err.Error())
	}

	cooldown := 30
	if cooldownSeconds != "" {
		cooldown, err = strconv.Atoi(cooldownSeconds)
		if err != nil {
			log.Printf("WARN - The provided cooldownPeriod=%s is invalid, using default value=%v", cooldownSeconds, cooldown)
		}
	}

	kapi := client.NewKeysAPI(etcd)
	notifier := newNotifier(kapi, "/ft/services/")

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	for {
		s := time.Now()
		log.Println("rebuilding configuration")
		// since vcb reads all the changes made in etcd, all notifications still in the channel can be ignored.
		drainChannel(notifier.notify())
		log.Printf("drained notifications channel")

		applyVulcanConf(kapi, buildVulcanConf(readServices(kapi)))
		log.Printf("completed reconfiguration. %v\n", time.Now().Sub(s))

		// wait for a change
		select {
		case <-c:
			log.Println("exiting")
			return
		case <-notifier.notify():
		}

		log.Printf("change detected, waiting in cooldown period for %v seconds", cooldown)
		<-time.After(time.Duration(cooldown) * time.Second)
	}

}

func drainChannel(ch <-chan struct{}) {
	drain := true
	for drain {
		select {
		case <-ch:
		default:
			drain = false
		}
	}
}

type Service struct {
	Name              string
	HasHealthCheck    bool
	Addresses         map[string]string
	PathPrefixes      map[string]string
	PathHosts         map[string]string
	FailoverPredicate string
}

func readServices(kapi client.KeysAPI) []Service {
	resp, err := kapi.Get(context.Background(), "/ft/services/", &client.GetOptions{Recursive: true})
	if err != nil {
		log.Println("error reading etcd keys")
		if e, _ := err.(client.Error); e.Code == etcderr.EcodeKeyNotFound {
			log.Println("core key not found")
			return []Service{}
		}
		log.Panicf("failed to read from etcd: %v\n", err.Error())
	}
	if !resp.Node.Dir {
		log.Panicf("%v is not a directory", resp.Node.Key)
	}

	var services []Service
	for _, node := range resp.Node.Nodes {
		if !node.Dir {
			log.Printf("skipping non-directory %v\n", node.Key)
			continue
		}
		service := Service{
			Name:         filepath.Base(node.Key),
			Addresses:    make(map[string]string),
			PathPrefixes: make(map[string]string),
			PathHosts:    make(map[string]string),
		}
		for _, child := range node.Nodes {
			switch filepath.Base(child.Key) {
			case "healthcheck":
				service.HasHealthCheck = child.Value == "true"
			case "servers":
				for _, server := range child.Nodes {
					service.Addresses[filepath.Base(server.Key)] = server.Value
				}
			case "path-regex":
				for _, path := range child.Nodes {
					service.PathPrefixes[filepath.Base(path.Key)] = path.Value
				}
			case "path-host":
				for _, path := range child.Nodes {
					service.PathHosts[filepath.Base(path.Key)] = path.Value
				}
			case "failover-predicate":
				service.FailoverPredicate = child.Value
			default:
				fmt.Printf("skipped key %v for node %v\n", child.Key, child)
			}
		}
		services = append(services, service)
	}
	return services
}

type vulcanConf struct {
	FrontEnds map[string]vulcanFrontend
	Backends  map[string]vulcanBackend
}

type vulcanFrontend struct {
	BackendID         string
	Route             string
	Type              string
	rewrite           vulcanRewrite
	FailoverPredicate string
}

type vulcanRewrite struct {
	ID         string
	Type       string
	Priority   int
	Middleware vulcanRewriteMw
}

type vulcanRewriteMw struct {
	Regexp      string
	Replacement string
}

type vulcanBackend struct {
	Servers map[string]vulcanServer
}

type vulcanServer struct {
	URL string
}

func buildVulcanConf(services []Service) vulcanConf {
	vc := vulcanConf{
		Backends:  make(map[string]vulcanBackend),
		FrontEnds: make(map[string]vulcanFrontend),
	}

	for _, service := range services {

		// "main" backend
		mainBackend := vulcanBackend{Servers: make(map[string]vulcanServer)}
		backendName := fmt.Sprintf("vcb-%s", service.Name)
		for svrID, sa := range service.Addresses {
			if addressRegex.MatchString(sa) {
				mainBackend.Servers[svrID] = vulcanServer{sa}
			} else {
				log.Printf("Skipping invalid backend address: %v for service %s\n", sa, service.Name)
			}

		}
		vc.Backends[backendName] = mainBackend

		// Host header front end
		frontEndName := fmt.Sprintf("vcb-byhostheader-%s", service.Name)
		vc.FrontEnds[frontEndName] = vulcanFrontend{
			Type:              "http",
			BackendID:         backendName,
			Route:             fmt.Sprintf("PathRegexp(`/.*`) && Host(`%s`)", service.Name),
			FailoverPredicate: service.FailoverPredicate,
		}

		// instance backends
		for svrID, sa := range service.Addresses {
			instanceBackend := vulcanBackend{Servers: make(map[string]vulcanServer)}
			if addressRegex.MatchString(sa) {
				instanceBackend.Servers[svrID] = vulcanServer{sa}
			} else {
				log.Printf("Skipping invalid backend address: %v for service %s\n", sa, service.Name)
			}
			backendName := fmt.Sprintf("vcb-%s-%s", service.Name, svrID)
			vc.Backends[backendName] = instanceBackend
		}

		// health check front ends
		if service.HasHealthCheck {
			for svrID := range service.Addresses {
				frontEndName := fmt.Sprintf("vcb-health-%s-%s", service.Name, svrID)
				backendName := fmt.Sprintf("vcb-%s-%s", service.Name, svrID)

				vc.FrontEnds[frontEndName] = vulcanFrontend{
					Type:      "http",
					BackendID: backendName,
					Route:     fmt.Sprintf("Path(`/health/%s-%s/__health`)", service.Name, svrID),
					rewrite: vulcanRewrite{
						ID:       "rewrite",
						Type:     "rewrite",
						Priority: 1,
						Middleware: vulcanRewriteMw{
							Regexp:      fmt.Sprintf("/health/%s-%s(.*)", service.Name, svrID),
							Replacement: "$1",
						},
					},
				}

			}
		}

		// internal frontend
		internalFrontEndName := fmt.Sprintf("vcb-internal-%s", service.Name)
		vc.FrontEnds[internalFrontEndName] = vulcanFrontend{
			Type:      "http",
			BackendID: backendName,
			Route:     fmt.Sprintf("PathRegexp(`/__%s/.*`)", service.Name),
			rewrite: vulcanRewrite{
				ID:       "rewrite",
				Type:     "rewrite",
				Priority: 1,
				Middleware: vulcanRewriteMw{
					Regexp:      fmt.Sprintf("/__%s(/.*)", service.Name),
					Replacement: "$1",
				},
			},
			FailoverPredicate: service.FailoverPredicate,
		}

		// public path front ends
		for pathName, pathRegex := range service.PathPrefixes {
			customHost, customHostExists := service.PathHosts[pathName]
			var route string
			if customHostExists {
				route = fmt.Sprintf("PathRegexp(`%s`) && Host(`%s`)", pathRegex, customHost)
			} else {
				route = fmt.Sprintf("PathRegexp(`%s`)", pathRegex)
			}
			vc.FrontEnds[fmt.Sprintf("vcb-%s-path-regex-%s", service.Name, pathName)] = vulcanFrontend{
				Type:              "http",
				BackendID:         backendName,
				Route:             route,
				FailoverPredicate: service.FailoverPredicate,
			}
		}
	}

	return vc
}

func applyVulcanConf(kapi client.KeysAPI, vc vulcanConf) {

	newConf := vulcanConfToEtcdKeys(vc)

	existing, err := readAllKeysFromEtcd(kapi, "/vulcand/")
	if err != nil {
		panic(err)
	}

	for k, v := range existing {
		// keep the keys not created by us
		if !strings.HasPrefix(k, "/vulcand/backends/vcb-") && !strings.HasPrefix(k, "/vulcand/frontends/vcb-") {
			newConf[k] = v
		}
	}

	changed := false
	// remove unwanted frontends
	for k := range existing {
		if strings.HasPrefix(k, "/vulcand/frontends/vcb-") {
			_, found := newConf[k]
			if !found {
				changed = true
				log.Printf("deleting frontend %s\n", k)
				_, err := kapi.Delete(context.Background(), k, &client.DeleteOptions{Recursive: false})
				if err != nil {
					log.Printf("error deleting frontend %v\n", k)
				}
			}
		}
	}

	// remove unwanted backends
	for k := range existing {
		if strings.HasPrefix(k, "/vulcand/backends/vcb-") {
			_, found := newConf[k]
			if !found {
				changed = true
				log.Printf("deleting backend%s\n", k)
				_, err := kapi.Delete(context.Background(), k, &client.DeleteOptions{Recursive: false})
				if err != nil {
					log.Printf("error deleting backend %v\n", k)
				}
			}
		}
	}

	// add or modify backends
	for k, v := range newConf {
		if strings.HasPrefix(k, "/vulcand/backends") {
			oldVal := existing[k]
			if v != oldVal {
				changed = true
				log.Printf("setting backend %s to %s\n", k, v)
				if _, err := kapi.Set(context.Background(), k, v, nil); err != nil {
					log.Printf("error setting %s to %s\n", k, v)
				}
			}
		}
	}

	// add or modify frontends
	for k, v := range newConf {
		if strings.HasPrefix(k, "/vulcand/frontends") && !strings.HasSuffix(k, "/middlewares/rewrite") {
			oldVal := existing[k]
			if v != oldVal {
				changed = true
				log.Printf("setting frontend %s to %s\n", k, v)
				if _, err := kapi.Set(context.Background(), k, v, nil); err != nil {
					log.Printf("error setting %s to %s\n", k, v)
				}
			}
		}
	}

	// add or modify everything else
	for k, v := range newConf {
		oldVal := existing[k]
		if v != oldVal {
			changed = true
			log.Printf("setting %s to %s\n", k, v)
			if _, err := kapi.Set(context.Background(), k, v, nil); err != nil {
				log.Printf("error setting %s to %s\n", k, v)
			}
		}
	}

	log.Printf("changes occured in etcd: %t ", changed)
	// some cleanup of known possible empty directories
	cleanFrontends(kapi)
	cleanBackends(kapi)
}

func cleanFrontends(kapi client.KeysAPI) {

	resp, err := kapi.Get(context.Background(), "/vulcand/frontends/", &client.GetOptions{Recursive: true})
	if err != nil {
		if e, _ := err.(client.Error); e.Code == etcderr.EcodeKeyNotFound {
			return
		}
		panic(err)
	}
	if !resp.Node.Dir {
		log.Println("/vulcand/frontends is not a directory.")
		return
	}
	for _, fe := range resp.Node.Nodes {
		feHasContent := false
		if fe.Dir {
			for _, child := range fe.Nodes {
				// anything apart from an empty "middlewares" dir means this is needed.
				if filepath.Base(child.Key) != "middlewares" || len(child.Nodes) > 0 {
					feHasContent = true
					break
				}
			}
		}
		if !feHasContent {
			_, err := kapi.Delete(context.Background(), fe.Key, &client.DeleteOptions{Recursive: true})
			if err != nil {
				log.Printf("failed to remove unwanted frontend %v\n", fe.Key)
			}
		}
	}

}

func cleanBackends(kapi client.KeysAPI) {

	resp, err := kapi.Get(context.Background(), "/vulcand/backends/", &client.GetOptions{Recursive: true})
	if err != nil {
		if e, _ := err.(client.Error); e.Code == etcderr.EcodeKeyNotFound {
			return
		}
		panic(err)
	}
	if !resp.Node.Dir {
		log.Println("/vulcand/backends is not a directory.")
		return
	}
	for _, be := range resp.Node.Nodes {
		beHasContent := false
		if be.Dir {
			for _, child := range be.Nodes {
				// anything apart from an empty "servers" dir means this is needed.
				if filepath.Base(child.Key) != "servers" || len(child.Nodes) > 0 {
					beHasContent = true
					break
				}
			}
		}
		if !beHasContent {
			_, err := kapi.Delete(context.Background(), be.Key, &client.DeleteOptions{Recursive: true})
			if err != nil {
				log.Printf("failed to remove unwanted backend %v\n", be.Key)
			}
		}
	}

}

func vulcanConfToEtcdKeys(vc vulcanConf) map[string]string {
	m := make(map[string]string)

	// create backends
	for beName, be := range vc.Backends {
		k := fmt.Sprintf("/vulcand/backends/%s/backend", beName)
		v := `{"Type": "http", "Settings": {"KeepAlive": {"MaxIdleConnsPerHost": 256, "Period": "35s"}}}`
		m[k] = v

		for sName, s := range be.Servers {
			k := fmt.Sprintf("/vulcand/backends/%s/servers/%s", beName, sName)
			v := fmt.Sprintf(`{"url":"%s"}`, s.URL)
			m[k] = v
		}

	}

	// create frontends
	for feName, be := range vc.FrontEnds {
		k := fmt.Sprintf("/vulcand/frontends/%s/frontend", feName)
		v := fmt.Sprintf(`{"Type":"%s", "BackendId":"%s", "Route":"%s", "Settings": {"FailoverPredicate":"%s"}}`, be.Type, be.BackendID, be.Route, be.FailoverPredicate)
		m[k] = v
		if be.rewrite.ID != "" {
			k := fmt.Sprintf("/vulcand/frontends/%s/middlewares/rewrite", feName)
			v := fmt.Sprintf(

				`{"Id":"%s", "Type":"%s", "Priority":%d, "Middleware": {"Regexp":"%s", "Replacement":"%s"}}`,
				be.rewrite.ID,
				be.rewrite.Type,
				be.rewrite.Priority,
				be.rewrite.Middleware.Regexp,
				be.rewrite.Middleware.Replacement,
			)
			m[k] = v
		}
	}

	return m
}

func newNotifier(kapi client.KeysAPI, path string) notifier {
	w := notifier{make(chan struct{}, 1)}

	go func() {

		for {
			watcher := kapi.Watcher(path, &client.WatcherOptions{Recursive: true})

			var err error
			var response *client.Response
			for err == nil {
				response, err = watcher.Next(context.Background())
				logResponse(response)
				select {
				case w.ch <- struct{}{}:
					log.Println("received event from watcher, sent change message on notifier channel.")
				default:
					log.Println("received event from watcher, not sending message on notifier channel, buffer full and no-one listening.")
				}
			}

			if err == context.Canceled {
				log.Println("context cancelled error")
			} else if err == context.DeadlineExceeded {
				log.Println("deadline exceeded error")
			} else if cerr, ok := err.(*client.ClusterError); ok {
				log.Printf("cluster error. Details: %v\n", cerr.Detail())
			} else {
				// bad cluster endpoints, which are not etcd servers
				log.Println(err.Error())
			}

			log.Println("sleeping for 15s before rebuilding config due to error")
			time.Sleep(15 * time.Second)
		}
	}()

	return w
}

func logResponse(response *client.Response) {
	if response == nil {
		return
	}
	log.Println("Event from watcher:")
	log.Printf("Action: %s\n", response.Action)
	if response.PrevNode != nil {
		log.Printf("Old key:value  %s:%s\n", response.PrevNode.Key, response.PrevNode.Value)
	}
	if response.Node != nil {
		log.Printf("New key:value  %s:%s\n", response.Node.Key, response.Node.Value)
	}
}

type notifier struct {
	ch chan struct{}
}

func (w *notifier) notify() <-chan struct{} {
	return w.ch
}

func readAllKeysFromEtcd(kapi client.KeysAPI, root string) (map[string]string, error) {
	m := make(map[string]string)

	resp, err := kapi.Get(context.Background(), root, &client.GetOptions{Recursive: true})
	if err != nil {
		if e, _ := err.(client.Error); e.Code == etcderr.EcodeKeyNotFound {
			return m, nil
		}
		panic(err)
	}
	addAllValuesToMap(m, resp.Node)
	return m, nil
}

func addAllValuesToMap(m map[string]string, node *client.Node) {
	if node.Dir {
		for _, child := range node.Nodes {
			addAllValuesToMap(m, child)
		}
	} else {
		m[node.Key] = node.Value
	}
}
