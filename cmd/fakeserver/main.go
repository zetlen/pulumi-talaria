// Command fakeserver runs the in-memory iac protocol server used by scripts/e2e.sh.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/zetlen/pulumi-talaria/internal/fake"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "listen address")
	key := flag.String("api-key", "test-key", "accepted API key")
	flag.Parse()
	log.Printf("fake talaria iac server on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, fake.New(*key)))
}
