#!/usr/bin/env bash
# Runs the sbxr DNS matrix and self-kill checks in kind.
set -euo pipefail

NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.37.0}
NODE=sbxr-control-plane
HOSTS=(kubernetes.default.svc.cluster.local example.com)
IMAGES=(debian:bookworm-slim alpine:3.22 busybox:1.37 dnsgo:1)
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}

log() { printf '\n=== %s\n' "$*"; }

create_cluster() {
  local gates=$1
  local cfg=/tmp/kind.yaml
  cat >"$cfg" <<CFG
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /dev/kvm
        containerPath: /dev/kvm
CFG
  if [[ $gates == yes ]]; then
    cat >>"$cfg" <<CFG
featureGates:
  MutatingAdmissionPolicy: true
runtimeConfig:
  admissionregistration.k8s.io/v1beta1: "true"
  admissionregistration.k8s.io/v1alpha1: "true"
CFG
  fi
  cat "$cfg"

  kind delete cluster --name sbxr >/dev/null 2>&1 || true
  kind create cluster --name sbxr --image "$NODE_IMAGE" --config "$cfg" --wait 120s
  sudo chmod 666 /dev/kvm

  docker cp /tmp/krun-out/. "$NODE:/usr/local/"
  docker exec "$NODE" sh -c 'ldconfig && ln -sf /usr/local/bin/crun /usr/local/bin/krun && ldd /usr/local/bin/crun && crun --version && ls -la /dev/kvm'
  docker exec "$NODE" sh -c 'cat >>/etc/containerd/config.toml <<TOML

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxr]
  runtime_type = "io.containerd.runc.v2"
  [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.sbxr.options]
    BinaryName = "/usr/local/bin/krun"
TOML
systemctl restart containerd'
  sleep 5
  docker exec "$NODE" sh -c 'containerd config dump | grep -A4 "runtimes.sbxr"'

  kubectl apply -f - <<YAML
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: sbxr
handler: sbxr
YAML
  kubectl wait --for=condition=Ready node --all --timeout=120s
  kubectl -n kube-system rollout status deploy/coredns --timeout=120s
  kind load docker-image --name sbxr "${IMAGES[@]}"
}

# run_pod NAME RUNTIMECLASS DNSCONFIG_JSON IMAGE CMD...
run_pod() {
  local name=$1 rc=$2 dns=$3 image=$4
  shift 4
  jq -n --arg name "$name" --arg rc "$rc" --argjson dns "$dns" --arg image "$image" '{
    apiVersion: "v1", kind: "Pod", metadata: {name: $name},
    spec: ({restartPolicy: "Never", terminationGracePeriodSeconds: 1,
      containers: [{name: "c", image: $image, imagePullPolicy: "IfNotPresent", command: $ARGS.positional}]}
      + (if $rc == "" then {} else {runtimeClassName: $rc} end)
      + (if $dns == null then {} else {dnsConfig: $dns} end))
  }' --args "$@" | kubectl apply -f -
}

wait_pod() {
  local phase
  for _ in $(seq 240); do
    phase=$(kubectl get pod "$1" -o jsonpath='{.status.phase}')
    if [[ $phase == Succeeded || $phase == Failed ]]; then
      return 0
    fi
    sleep 1
  done
  echo "TIMEOUT waiting for $1 (phase=$phase)"
  kubectl describe pod "$1" | tail -20
}

report_pod() {
  local name=$1
  wait_pod "$name"
  log "pod $name"
  kubectl get pod "$name" -o jsonpath='runtimeClassName={.spec.runtimeClassName} dnsConfig={.spec.dnsConfig}{"\n"}terminated={.status.containerStatuses[0].state.terminated}{"\n"}'
  kubectl logs "$name" || true
  kubectl logs "$name" 2>/dev/null | grep -E '^(OK|FAIL) ' | sed "s/^/RESULT $name: /" | tee -a /tmp/results.txt || true
}

# matrix MODE RUNTIMECLASS DNSCONFIG_JSON
matrix() {
  local mode=$1 rc=$2 dns=$3
  local loop='cat /etc/resolv.conf; rc=0; for h in '"${HOSTS[*]}"'; do s=$(date +%s); if timeout 30 '
  local tail=' "$h"; then echo "OK $h $(( $(date +%s) - s ))s"; else echo "FAIL $h exit=$? $(( $(date +%s) - s ))s"; rc=1; fi; done; exit $rc'
  run_pod "$mode-go" "$rc" "$dns" dnsgo:1 /dnsgo "${HOSTS[@]}"
  run_pod "$mode-glibc" "$rc" "$dns" debian:bookworm-slim sh -c "${loop}getent hosts${tail}"
  run_pod "$mode-musl" "$rc" "$dns" alpine:3.22 sh -c "${loop}getent hosts${tail}"
  run_pod "$mode-busybox" "$rc" "$dns" busybox:1.37 sh -c "${loop}nslookup${tail}"
  for c in go glibc musl busybox; do
    report_pod "$mode-$c"
  done
}

mapolicy_version() {
  local v
  for v in v1 v1beta1 v1alpha1; do
    if kubectl get --raw "/apis/admissionregistration.k8s.io/$v" 2>/dev/null | jq -e '.resources[] | select(.name == "mutatingadmissionpolicies")' >/dev/null; then
      echo "$v"
      return 0
    fi
  done
}

install_policy() {
  local v=$1
  cat >/tmp/policy.yaml <<YAML
apiVersion: admissionregistration.k8s.io/$v
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
apiVersion: admissionregistration.k8s.io/$v
kind: MutatingAdmissionPolicyBinding
metadata:
  name: sbxr-dns-use-vc
spec:
  policyName: sbxr-dns-use-vc
YAML
  cat /tmp/policy.yaml
  kubectl apply -f /tmp/policy.yaml

  for _ in $(seq 30); do
    if run_dry probe sbxr null | grep -q use-vc; then
      echo "policy active"
      return 0
    fi
    sleep 2
  done
  echo "policy NOT active after 60s"
}

run_dry() {
  local name=$1 rc=$2 dns=$3
  jq -n --arg name "$name" --arg rc "$rc" --argjson dns "$dns" '{
    apiVersion: "v1", kind: "Pod", metadata: {name: $name},
    spec: ({containers: [{name: "c", image: "busybox:1.37"}]}
      + (if $rc == "" then {} else {runtimeClassName: $rc} end)
      + (if $dns == null then {} else {dnsConfig: $dns} end))
  }' | kubectl create --dry-run=server -o jsonpath='{.spec.dnsConfig}' -f -
}

main() {
  : >/tmp/results.txt
  create_cluster no
  kubectl version
  log "api-resources (no gates)"
  kubectl api-resources | grep -i mutatingadmission || echo "none served"
  local v
  v=$(mapolicy_version || true)
  if [[ -z $v ]]; then
    log "MutatingAdmissionPolicy not served by default, recreating cluster with gates"
    create_cluster yes
    kubectl api-resources | grep -i mutatingadmission || echo "none served"
    v=$(mapolicy_version || true)
  fi
  echo "MAP_VERSION=${v:-none}" | tee -a /tmp/results.txt

  log "matrix: runc control"
  matrix runc "" null
  log "matrix: sbxr no mitigation"
  matrix none sbxr null
  log "matrix: sbxr pod dnsConfig use-vc"
  matrix podopt sbxr '{"options":[{"name":"use-vc"}]}'

  if [[ -n $v ]]; then
    log "policy"
    install_policy "$v"
    log "policy dry-run checks"
    echo "sbxr, no dnsConfig:      $(run_dry d1 sbxr null)" | tee -a /tmp/results.txt
    echo "sbxr, ndots:2:           $(run_dry d2 sbxr '{"options":[{"name":"ndots","value":"2"}]}')" | tee -a /tmp/results.txt
    echo "sbxr, nameservers only:  $(run_dry d3 sbxr '{"searches":["x.local"]}')" | tee -a /tmp/results.txt
    echo "sbxr, use-vc present:    $(run_dry d4 sbxr '{"options":[{"name":"use-vc"}]}')" | tee -a /tmp/results.txt
    echo "runc, no dnsConfig:      $(run_dry d5 "" null)" | tee -a /tmp/results.txt
    log "matrix: sbxr policy"
    matrix policy sbxr null
  fi

  log "self-kill"
  run_pod kill9-sbxr sbxr null alpine:3.22 sh -c 'echo pid=$$$$; kill -9 $$$$; echo survived'
  run_pod kill9-runc "" null alpine:3.22 sh -c 'echo pid=$$$$; kill -9 $$$$; echo survived'
  run_pod segv-sbxr sbxr null alpine:3.22 sh -c 'echo pid=$$$$; kill -SEGV $$$$; echo survived'
  run_pod segv-runc "" null alpine:3.22 sh -c 'echo pid=$$$$; kill -SEGV $$$$; echo survived'
  for p in kill9-sbxr kill9-runc segv-sbxr segv-runc; do
    wait_pod "$p"
    log "pod $p"
    kubectl logs "$p" || true
    echo "SELFKILL $p: $(kubectl get pod "$p" -o jsonpath='{.status.phase} {.status.containerStatuses[0].state.terminated}')" | tee -a /tmp/results.txt
  done

  log "RESULTS"
  cat /tmp/results.txt
  { echo '```'; cat /tmp/results.txt; echo '```'; } >>"$SUMMARY"
}

main "$@"
