#!/bin/bash

set -xeou pipefail

trap "exit" INT TERM ERR

echo "PID of this wrapper script: $$"

# download kubernetes source code
latest=$(curl -s https://api.github.com/repos/kubernetes/kubernetes/releases/latest | grep "tag_name" | cut -d : -f 2,3 | tr -d ,| tr -d " " | tr -d "\"")
echo "kubernetes latest version: $latest"

curl -o /tmp/kubernetes-*.tar.gz -L "https://github.com/kubernetes/kubernetes/archive/refs/tags/$latest.tar.gz" && tar -xvf /tmp/kubernetes-*.tar.gz -C /tmp && cp -R /tmp/kubernetes-*/ ./kubernetes

#run make on kubernetes
cd kubernetes
make

cd ../

mkdir -p ./vendor/k8s.io/kubernetes

#cp -R ./kubernetes-*/staging/src ./pkg/kubernetes
cp -R ./kubernetes/pkg ./vendor/k8s.io/kubernetes
cp -R ./kubernetes/cmd ./vendor/k8s.io/kubernetes


#use tool to rename in entire project
../go-imports-rename/go-imports-rename --save 'k8s.io/kubernetes/ => github.com/pachyderm/fakeapiserver/pkg/k8s.io/kubernetes/'

# build the fake apiserver
go build cmd/test-apiserver/main.go