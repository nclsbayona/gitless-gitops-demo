package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	cowsay "github.com/Code-Hex/Neo-cowsay/v2"
)

func salute(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	var req_url url.URL = *req_url
	q := req_url.Query()
	q.Set("name", name)
	req_url.RawQuery = q.Encode()
	hello_response, e := http.Get(req_url.String())
	if e != nil {
		fmt.Println("Error getting response from "+req_url.String())
	}
	defer hello_response.Body.Close()
	body, e := io.ReadAll(hello_response.Body)
	if e != nil {
		fmt.Println("Error reading response from "+req_url.String())
	}
	response, e := cow.Say(string(body))
	if e != nil {
		fmt.Println("Error with cowsay "+string(body))
	}
	fmt.Println(response)
	io.WriteString(w, response)
}

func alive(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
}

var cow *cowsay.Cow
var req_url *url.URL

func init() {
	var err error
	cow, err = cowsay.New(
		cowsay.BallonWidth(40),
	)

	if err != nil {
		os.Exit(1)
	}

	server_url := os.Getenv("HELLO_SERVER_URL")
	req_url, err = url.Parse(server_url)
	req_url.Path += "/hello"
	
	if err != nil {
		os.Exit(1)
	}
}

func main() {
	http.HandleFunc("/", alive)
	http.HandleFunc("/hello", salute)

	_ = http.ListenAndServe(":8080", nil)
}
