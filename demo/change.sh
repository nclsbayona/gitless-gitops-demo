#!/bin/bash


echo "Before changes"
kubectl get deployments -n dev -o custom-columns='NAME:.metadata.name,IMAGE:.spec.template.spec.containers[*].image'
echo "Waiting for pods to be ready..."
while true; do
    NOT_READY=$(kubectl get pods -n dev --no-headers | grep -v "Running" | wc -l)
    if [ "$NOT_READY" -eq 0 ]; then
        break
    fi
    echo "Still waiting for $NOT_READY pods to be ready..."
    sleep 5
done
echo "All pods are running"
kubectl port-forward -n dev svc/ui 8080:80 &
echo "Open browser at http://localhost:8080/home and test"
read -p "Press Enter to continue..."

cat << 'EOF'
Change for cowsay service is adding -- as eyes instead of default oo
From:
cow, err = cowsay.New(
		cowsay.BallonWidth(40),
	)
To:
cow, err = cowsay.New(
		cowsay.BallonWidth(40),
		cowsay.Eyes("--"),
	)
EOF

cat << 'EOF' > ../definitions/app/microservices/cowsay/main.go
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
		cowsay.Eyes("--"),
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
EOF

sed -i 's/^ui=1\.0\.0/ui=1.0.1/' ../definitions/app/versions.txt

sh ../definitions/app/push-microservices.sh "ui" "zot.oci.svc.cluster.local"
sh ../definitions/app/push-app.sh "dev" "zot.oci.svc.cluster.local" "v1.0.1"

echo "Waiting for pods to be ready..."
while true; do
    NOT_READY=$(kubectl get pods -n dev --no-headers | grep -v "Running" | wc -l)
    if [ "$NOT_READY" -eq 0 ]; then
        break
    fi
    echo "Still waiting for $NOT_READY pods to be ready..."
    sleep 5
done
echo "All pods are running"
echo "After changes"
pkill -f "port-forward.*svc/ui"
kubectl get deployments -n dev -o custom-columns='NAME:.metadata.name,IMAGE:.spec.template.spec.containers[*].image'
kubectl port-forward -n dev svc/ui 8080:80 &
echo "Open browser at http://localhost:8080/home and test"
read -p "Press Enter to continue..."