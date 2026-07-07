# dex-idp-operator

## Description

`dex-idp-operator` manages Dex IDP configuration from Kubernetes custom
resources. The first supported workflow is declarative management of Dex OAuth2
clients through Dex's gRPC API.

The operator provides two namespaced APIs:

- `DexConnection` describes how to reach a Dex gRPC endpoint, including TLS
  material supplied inline or from Kubernetes Secrets.
- `DexClient` describes one Dex OAuth2 client and references a `DexConnection`.

Secrets can be provided either inline for local development or through
`SecretRef` fields for externally managed secrets. This applies to Dex client
secrets and to TLS material used to connect to Dex.

Dex can update client redirect URIs, trusted peers, name, and logo URL in place.
Dex does not update client secrets or the public/confidential setting through
its update API. When those immutable fields change, the operator marks the
`DexClient` as blocked unless `spec.recreateOnImmutableChange` is set to `true`.

## Getting Started

### Prerequisites
- go version v1.25.7+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### Example Resources

Create a reusable connection to Dex:

```yaml
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexConnection
metadata:
  name: dex-local
spec:
  address: dex.dex.svc.cluster.local:5557
  plaintext: true
```

Create a Dex OAuth2 client using an externally managed Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: example-app-dex-client
stringData:
  client-secret: change-me
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexClient
metadata:
  name: example-app
spec:
  connectionRef:
    name: dex-local
  clientID: example-app
  secret:
    valueFrom:
      secretKeyRef:
        name: example-app-dex-client
        key: client-secret
  redirectURIs:
    - https://app.example.com/callback
  name: Example App
```

For local development only, the client secret can also be supplied inline:

```yaml
spec:
  secret:
    value: change-me
```

For TLS-enabled Dex gRPC endpoints, provide CA and optional mTLS material:

```yaml
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexConnection
metadata:
  name: dex
spec:
  address: dex.dex.svc.cluster.local:5557
  tls:
    serverName: dex.dex.svc
    ca:
      valueFrom:
        secretKeyRef:
          name: dex-grpc-tls
          key: ca.crt
    clientCertificate:
      certificate:
        valueFrom:
          secretKeyRef:
            name: dex-grpc-tls
            key: tls.crt
      privateKey:
        valueFrom:
          secretKeyRef:
            name: dex-grpc-tls
            key: tls.key
```

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/dex-idp-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/dex-idp-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples from `config/samples`:

```sh
kubectl apply -k config/samples/
```

The sample connection uses plaintext gRPC for a local Dex service. Use TLS and
SecretRefs for production.

### Development

Regenerate manifests and generated code after changing API types or markers:

```sh
make manifests generate
```

Run the unit/envtest suite:

```sh
make test
```

Run e2e tests only against a dedicated Kind cluster:

```sh
make test-e2e
```

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/dex-idp-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/dex-idp-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## Contributing

Keep changes aligned with Kubebuilder conventions. Prefer `kubebuilder create
api` and `kubebuilder create webhook` for new API surfaces, keep generated files
generated, and add focused tests for reconciliation behavior.

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

