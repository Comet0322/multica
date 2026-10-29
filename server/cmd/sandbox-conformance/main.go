// sandbox-conformance checks a sandbox service against
// deploy/sandbox/CONTRACT.md. It creates and deletes sandboxes whose keys
// start with "conformance-".
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/multica-ai/multica/server/internal/sandboxprovider"
	"github.com/multica-ai/multica/server/internal/sandboxprovider/conformance"
)

func main() {
	serviceURL := flag.String("url", "", "sandbox service base URL")
	token := flag.String("token", os.Getenv("SANDBOX_SERVICE_TOKEN"), "service token (default $SANDBOX_SERVICE_TOKEN)")
	image := flag.String("image", "", "probe image built from deploy/sandbox/conformance")
	listen := flag.String("listen", ":8799", "address for probe reports")
	callback := flag.String("callback-url", "", "URL at which sandboxes reach -listen")
	egress := flag.String("egress-url", "", "a URL sandboxes must NOT reach, e.g. https://example.com")
	startTimeout := flag.Duration("start-timeout", 3*time.Minute, "maximum time from PUT to running")
	flag.Parse()
	if *serviceURL == "" || *token == "" || *image == "" || *callback == "" {
		fmt.Fprintln(os.Stderr, "usage: sandbox-conformance -url URL -token TOKEN -image PROBE_IMAGE -callback-url URL [-listen ADDR] [-egress-url URL]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	provider, err := sandboxprovider.NewHTTP(sandboxprovider.HTTPConfig{BaseURL: *serviceURL, Token: *token})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen for probe reports:", err)
		os.Exit(2)
	}
	collector := conformance.NewCollector()
	srv := &http.Server{Handler: collector, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	results := conformance.CheckHTTPAPI(ctx, *serviceURL, *token)
	results = append(results, conformance.Run(ctx, provider, collector, conformance.Config{
		Image: *image, CallbackURL: *callback, EgressURL: *egress, StartTimeout: *startTimeout,
	})...)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range results {
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Level, r.Name, r.Message)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if conformance.Failed(results) {
		fmt.Println("\nFAIL: the service does not meet the contract")
		os.Exit(1)
	}
	fmt.Println("\nPASS")
}
