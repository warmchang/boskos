# `boskosctl` - A CLI for Leasing Resources from Boskos

`boskosctl` is a minimal command-line utility for leasing resources from a `boskos` server.

## Install

The simplest way to install the `boskosctl` CLI is via go get:

```
GO111MODULE=on go get sigs.k8s.io/boskos/cmd/boskosctl
```

This will install `boskosctl` to `$(go env GOPATH)/bin/boskosctl`.

## Workflow

The workflow for using the command-line utility is:

```sh
# for clarity, common arguments are presented once
function boskosctlwrapper() {
    boskosctl --server-url "${boskos_server}" --owner-name "${identifier}" "${@}"
}

# create a new lease on a resource
resource="$( boskosctlwrapper acquire --type things --state new --target-state owned --timeout 30m )"

# release the resource when the script exits
function release() {
    local resource_name; resource_name="$( jq .name <<<"${resource}" )"
    boskosctlwrapper release --name "${resource_name}" --target-state dirty
}
trap release EXIT

# send a heartbeat in the background to keep the lease while using the resource
boskosctlwrapper heartbeat --resource "${resource}" &
```

Sending a heartbeat is necessary only when the `boskos/reaper` is deployed in the cluster and is reaping resources of the type that was leased.
