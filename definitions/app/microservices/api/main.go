package main

import (
	"fmt"
	"io"
	"net/http"
)

func salute(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "fellow user"
	}
	response := "Hello " + name + " !"
	fmt.Println(response)
	io.WriteString(w, response)
}

func alive(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
}


func main() {
	http.HandleFunc("/", alive)
	http.HandleFunc("/hello", salute)

	_ = http.ListenAndServe(":8080", nil)
}