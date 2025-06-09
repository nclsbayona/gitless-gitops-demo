package main

import (
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
)

var baseURL string

func main() {
	http.HandleFunc("/", alive)
	http.HandleFunc("/home", root)
	http.HandleFunc("/say", salute)

	http.ListenAndServe(":8080", nil)
}

func root(w http.ResponseWriter, r *http.Request) {
	tmpl := `
	<html>
	<head><title>Cowsay UI</title></head>
	<body>
		<h1>Cowsay Hello</h1>
		<form action="/say" method="get">
			<label>Name:</label>
			<input type="text" name="name" required />
			<input type="submit" value="Say Hello" />
		</form>
	</body>
	</html>
	`
	w.Write([]byte(tmpl))
}

var req_url *url.URL
var err error

func init(){
	baseURL = os.Getenv("COWSAY_SERVER_URL")
	if baseURL == "" {
		os.Exit(1)
	}
	req_url, err = url.Parse(baseURL)
	if err != nil {
		os.Exit(1)
	}
	req_url.Path += "/hello"
}

func alive(w http.ResponseWriter, r *http.Request){
	w.WriteHeader(200)
}

func salute(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	var req_url url.URL = *req_url
	q := req_url.Query()
	q.Set("name", name)
	req_url.RawQuery = q.Encode()

	resp, err := http.Get(req_url.String())
	if err != nil || resp.StatusCode != http.StatusOK {
		http.Error(w, "Failed to fetch cowsay message", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response", http.StatusInternalServerError)
		return
	}

	renderResult(w, string(body))
}

func renderResult(w http.ResponseWriter, speech string) {
	tmpl := `
	<html>
	<head><title>Cowsay Output</title></head>
	<body>
		<pre>{{.}}</pre>
		<a href="/home">← Back</a>
	</body>
	</html>
	`
	t := template.Must(template.New("result").Parse(tmpl))
	t.Execute(w, speech)
}