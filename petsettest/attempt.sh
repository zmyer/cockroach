#!/bin/bash
set -exu

# Clear your cluster before running this if you run into troubles.
minikube delete
minikube start
sleep 3 # the captain awakens

# Make persistent volumes and (correctly named) claims. We must create the
# claims here even though that sounds counter-intuitive. For details see
# https://github.com/kubernetes/contrib/pull/1295#issuecomment-230180894
for i in 0 1 2 3 4; do
  kubectl create -f local${i}.yaml
  kubectl create -f claim-datadir-${i}.yaml
done;

kubectl create -f petset.yaml
kubectl get pvc
