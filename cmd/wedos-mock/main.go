// Command wedos-mock runs the in-memory WEDOS REST API fake on a local port,
// for trying the provider with `terraform apply` without a WEDOS account.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/skip-pay/terraform-provider-wedos/internal/wedosmock"
)

func main() {
	zones := flag.String("zone", "", "comma-separated zones to pre-create")
	flag.Parse()

	s := wedosmock.New()
	defer s.Close()
	for _, z := range strings.FieldsFunc(*zones, func(r rune) bool { return r == ',' }) {
		s.AddZone(z)
	}

	fmt.Printf("export WEDOS_BASE_URL=%s WEDOS_CLIENT_ID=%s WEDOS_CLIENT_SECRET=%s\n", s.URL, s.ClientID, s.ClientSecret)
	fmt.Println("Mock is running; Ctrl+C to stop. State is in memory only.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
