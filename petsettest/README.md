# CockroachDB on Kubernetes as a PetSet

Note that this is an experiment and cannot be deployed with local storage
outside of single-node testing. The "correct" way to deploy CockroachDB
might be a DaemonSet.

## Prerequisites (OSX)

```shell
brew reinstall kubernetes-cli --devel
curl -Lo minikube https://storage.googleapis.com/minikube/releases/v0.4.0/minikube-darwin-amd64 && chmod +x minikube && sudo mv minikube /usr/local/bin/
```

## Setting up the cluster

Follow the steps in `./attempt.sh` (or simply run that file, but note that
it will recreate your `minikube` instance).

```shell
kubectl logs cockroachdb-N # 0 <= N <= 4
```

## Simulating failures

When all (or enough) nodes are up, simulate a failure like this (I don't know
why more than one `kill 1` is needed, but that's the way it is):

```shell
kubectl exec cockroachdb-0 -- /bin/bash -c "while true; do kill 1; done"
```
