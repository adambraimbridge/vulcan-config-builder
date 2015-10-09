package main

import (
	"flag"
	"fmt"
	"github.com/coreos/etcd/client"
	"golang.org/x/net/context"
	"golang.org/x/net/proxy"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	var (
		socksProxy = flag.String("socks-proxy", "", "Use specified SOCKS proxy (e.g. localhost:2323)")
		etcdPeers  = flag.String("etcd-peers", "http://localhost:2379", "Comma-separated list of addresses of etcd endpoints to connect to")
	)

	flag.Parse()

	watcher := newWatcher("/ft/services/", *socksProxy, strings.Split(*etcdPeers, ","))

	for {
		<-watcher.wait()
		fmt.Println("something changed")
	}

}

func newWatcher(path string, socksProxy string, etcdPeers []string) watcher {
	w := watcher{make(chan struct{})}

	go func() {
		transport := client.DefaultTransport

		if socksProxy != "" {
			dialer, _ := proxy.SOCKS5("tcp", socksProxy, nil, proxy.Direct)
			transport = &http.Transport{Dial: dialer.Dial}
		}

		cfg := client.Config{
			Endpoints:               etcdPeers,
			Transport:               transport,
			HeaderTimeoutPerRequest: time.Second,
		}

		etcd, err := client.New(cfg)
		if err != nil {
			log.Fatal("failed to start etcd client: %v\n", err.Error())
		}

		kapi := client.NewKeysAPI(etcd)
		watcher := kapi.Watcher(path, &client.WatcherOptions{Recursive: true})

		for {
			_, err := watcher.Next(context.Background())
			if err != nil {
				log.Printf("watch failed %v, sleeping for 1s\n", err.Error())
				time.Sleep(1 * time.Second)
			} else {
				w.ch <- struct{}{}
			}
		}
	}()

	return w
}

type watcher struct {
	ch chan struct{}
}

func (w *watcher) wait() <-chan struct{} {
	return w.ch
}
