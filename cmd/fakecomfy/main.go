// Command fakecomfy serves a fake ComfyUI API from an object_info.json, for
// trying comfy plan/apply without a GPU.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/erstaples/comfyform/internal/fakecomfy"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8188", "listen address")
	info := flag.String("object-info", "testdata/object_info.json", "object_info.json to serve")
	flag.Parse()
	b, err := os.ReadFile(*info)
	if err != nil {
		log.Fatal(err)
	}
	s, err := fakecomfy.New(b)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake ComfyUI on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, s))
}
