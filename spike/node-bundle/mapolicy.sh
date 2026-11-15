#!/usr/bin/env bash
# A8: checks which MutatingAdmissionPolicy API versions a default kind cluster serves, then tests the use-vc policy.
# Usage: mapolicy.sh <node-image>
set -euo pipefail

image=$1
kind create cluster --image "$image" --wait 120s
kubectl version

echo "api-versions: $(kubectl api-versions | grep admissionregistration | tr '\n' ' ')"
kubectl api-resources --api-group=admissionregistration.k8s.io -o wide

served=none
for v in v1 v1beta1 v1alpha1; do
  if kubectl get --raw "/apis/admissionregistration.k8s.io/$v" 2>/dev/null | grep -q '"mutatingadmissionpolicies"'; then
    served=$v
    break
  fi
done
echo "RESULT $image: MutatingAdmissionPolicy served at ${served} by default"
if [[ $served != v1 ]]; then
  exit 0
fi

kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: sbxr}
handler: runc
---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingAdmissionPolicy
metadata:
  name: sbxr-dns-use-vc
spec:
  failurePolicy: Fail
  reinvocationPolicy: IfNeeded
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: ["v1"]
        operations: ["CREATE"]
        resources: ["pods"]
  matchConditions:
    - name: sbxr-runtime
      expression: "has(object.spec.runtimeClassName) && object.spec.runtimeClassName == 'sbxr'"
    - name: no-use-vc-yet
      expression: "!has(object.spec.dnsConfig) || !has(object.spec.dnsConfig.options) || !object.spec.dnsConfig.options.exists(o, o.name == 'use-vc')"
  mutations:
    - patchType: JSONPatch
      jsonPatch:
        expression: >
          !has(object.spec.dnsConfig) ?
            [JSONPatch{op: "add", path: "/spec/dnsConfig", value: Object.spec.dnsConfig{options: [Object.spec.dnsConfig.options{name: "use-vc"}]}}] :
          !has(object.spec.dnsConfig.options) ?
            [JSONPatch{op: "add", path: "/spec/dnsConfig/options", value: [Object.spec.dnsConfig.options{name: "use-vc"}]}] :
            [JSONPatch{op: "add", path: "/spec/dnsConfig/options/-", value: Object.spec.dnsConfig.options{name: "use-vc"}}]
---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingAdmissionPolicyBinding
metadata:
  name: sbxr-dns-use-vc
spec:
  policyName: sbxr-dns-use-vc
YAML

dry() {
  kubectl create --dry-run=server -o jsonpath='{.spec.dnsConfig}' -f - <<YAML
apiVersion: v1
kind: Pod
metadata: {name: probe}
spec:
  $1
  containers: [{name: c, image: "busybox:1.37"}]
YAML
}

for _ in $(seq 30); do
  if dry 'runtimeClassName: sbxr' | grep -q use-vc; then
    break
  fi
  sleep 2
done
echo "RESULT sbxr, no dnsConfig: $(dry 'runtimeClassName: sbxr')"
echo "RESULT sbxr, ndots kept: $(dry 'runtimeClassName: sbxr
  dnsConfig: {options: [{name: ndots, value: "2"}]}')"
echo "RESULT sbxr, use-vc present: $(dry 'runtimeClassName: sbxr
  dnsConfig: {options: [{name: use-vc}]}')"
echo "RESULT no runtimeClass: $(dry '')"
