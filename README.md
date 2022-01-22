# fakeapiserver

`fakeapiserver` is a Kubernetes environment that runs in the same process as your unit tests. No
external dependencies! It runs the real Kubernetes `kube-apiserver`, `kube-controller-manager`, and
an `etcd` instance, and you can talk to it with `kubectl`. (Everything you'd expect to work works;
if there are problems you can `kubectl get events` to see what's going on. Services are populated
with endpoints when containers become ready. Controllers like ReplicationControllers and Deployments
create pods from manifests.) The only difference is that we implement a fake kubelet that runs
containers in Pods as goroutines. What this means is that you can test complicated Kubernetes
integrations as a unit test, instead of having to painstakingly create a Kubernetes cluster, build
container images, push the container images to the cluster, etc. It's fast!

## Disclaimer

This is not an official Pachyderm product.

Additionally, this project is super early pre-alpha. I don't even know if it's going to be useful
yet.

## How to use

It's a mess right now. You'll need to check out Kubernetes to `../kubernetes` and run `make` there
(to generate the OpenAPI specs). Your eyeballs will explode if you look at `go.mod`.

## Limitations

Features necessary for CRD development are missing; we don't run an API extension server, for
exmaple.

We only start one node for now.

We implement our own version of the scheduler. Every pod that is created is immediately assigned to
`localhost` and started. No checks for compatability (hostPort, etc.) are done.

Multiple replicas cannot bind the same port; everything is running on localhost and you can only
listen on a port once. (This can be worked around in Linux if we make each node a different
127.X.Y.Z address and support multiple nodes. I'm thinking about it, but it doesn't work on OS X,
which will be annoying for some people.)
