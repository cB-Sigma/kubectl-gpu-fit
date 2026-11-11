// Command fake-cluster serves an imitation Kubernetes API server with a fake
// Prometheus behind its service proxy, and writes a kubeconfig for it. It
// lets you run the real kubectl-gpu-fit binary end to end on a laptop:
//
//	go run ./hack/fake-cluster -kubeconfig /tmp/gpu-fit.kubeconfig &
//	kubectl-gpu-fit --kubeconfig /tmp/gpu-fit.kubeconfig -A
//
// The data is invented. For development only.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/cb-Sigma/kubectl-gpu-fit/internal/kubetest"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/promtest"
	"github.com/cb-Sigma/kubectl-gpu-fit/internal/testcluster"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:0", "address to listen on")
	kubeconfig := flag.String("kubeconfig", "fake-cluster.kubeconfig", "where to write the kubeconfig")
	exported := flag.Bool("exported-labels", false, "serve DCGM series with exported_pod/exported_namespace labels")
	flag.Parse()

	objs, series := testcluster.New(time.Now(), *exported)
	fakeProm := promtest.New("7d", series) // lives until the process exits
	api := kubetest.NewAPIServer(objs...)
	api.Backends[testcluster.PrometheusNamespace+"/"+testcluster.PrometheusService] = fakeProm.Handler()

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	if err := os.WriteFile(*kubeconfig, []byte(testcluster.Kubeconfig(url, "default")), 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("fake API server:  %s\nfake Prometheus:  %s\nkubeconfig:       %s\n\ntry: kubectl-gpu-fit --kubeconfig %s -A\n",
		url, fakeProm.URL, *kubeconfig, *kubeconfig)

	server := &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(server.Serve(ln))
}
