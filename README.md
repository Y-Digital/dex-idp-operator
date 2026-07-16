# dex-idp-operator

## Description

`dex-idp-operator` manages Dex IDP configuration from Kubernetes custom
resources: OAuth2 clients through Dex's gRPC API, per-tenant Dex instances,
and the upstream IDP connectors those instances authenticate against.

The operator provides four namespaced APIs:

- `DexConnection` describes how to reach a Dex gRPC endpoint, including TLS
  material supplied inline or from Kubernetes Secrets.
- `DexClient` describes one Dex OAuth2 client. It targets either a single
  `DexConnection`, or fans out onto multiple `DexTenant`s (see
  [Multi-tenant routing](#multi-tenant-routing) below).
- `DexTenant` describes an operator-managed Dex `Deployment`/`Service` for one
  tenant, with its own issuer and upstream IDP connector.
- `DexConnector` describes one Dex connector's native configuration (OIDC,
  LDAP, GitHub, SAML, etc.), with `SecretRef` support for interpolating
  secret values into the connector config.

Secrets can be provided either inline for local development or through
`SecretRef` fields for externally managed secrets. This applies to Dex client
secrets, connector config secrets, and TLS material used to connect to Dex.

Dex can update client redirect URIs, trusted peers, name, and logo URL in place.
Dex does not update client secrets or the public/confidential setting through
its update API. When those immutable fields change, the operator marks the
`DexClient` as blocked unless `spec.recreateOnImmutableChange` is set to `true`.

### Multi-tenant routing

A common use case is a backend application that performs a single OIDC
exchange with Dex, where the specific Dex instance (and therefore the
upstream IDP) a request authenticates against depends on which tenant issuer
URL the request targets. The operator supports this pattern natively:

- Create one `DexTenant` per tenant. Each `DexTenant` gets its own managed Dex
  `Deployment`, `Service`, and issuer, and references exactly one
  `DexConnector` as its upstream IDP. The operator pre-seeds Dex's Kubernetes
  storage with that connector before the Dex `Deployment` starts, then keeps
  it in sync afterwards over Dex's gRPC API.
- Create one `DexClient` with `spec.tenantRefs` instead of `spec.connectionRef`
  to mount the same OAuth2 client (same `clientID`) onto every listed tenant.
  A confidential client (`public: false`) mounted this way gets a distinct,
  operator-generated client secret per tenant mount, recorded in
  `status.mounts` alongside the per-tenant sync status.
- The tenant's issuer URL is the routing key: whichever issuer a request
  authenticates against determines which upstream IDP handles it, since each
  tenant Dex instance only knows about its own connector.

**Important:** managed tenants currently use Dex's Kubernetes storage
backend, which scopes all state—including which connectors are available for
login—to the Kubernetes namespace the Dex `Deployment` runs in. Two
`DexTenant`s sharing a namespace would therefore share connector storage and
leak each other's upstream IDPs, defeating the point of per-tenant routing.
**Each `DexTenant` must run in its own dedicated namespace.** Because of this,
`spec.tenantRefs` entries on a `DexClient` accept an optional `namespace`
field (defaulting to the `DexClient`'s own namespace) so a single client can
mount onto tenants that live in different namespaces. See
[`config/samples/idp_v1alpha1_dexclient_multitenant.yaml`](config/samples/idp_v1alpha1_dexclient_multitenant.yaml)
for a complete example with two tenants in two namespaces.

Tenant exposure is Service-only: the operator does not create Ingress or
Gateway resources, so route the tenant's `spec.issuer` URL to its Service
using whatever ingress mechanism your cluster uses. Tenant Dex gRPC traffic is
internal plaintext for this MVP; only reach it from within the cluster.

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

Create a tenant-scoped Dex instance backed by its own upstream OIDC connector:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: tenant-a-oidc-secret
  namespace: tenant-a
stringData:
  clientSecret: change-me
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexConnector
metadata:
  name: tenant-a-connector
  namespace: tenant-a
spec:
  connectorID: upstream-oidc
  type: oidc
  displayName: Tenant A IDP
  config:
    issuer: https://idp-a.example.com
    clientID: dex
    clientSecret: ${oidcClientSecret}
    redirectURI: https://tenant-a.example.com/dex/callback
  secretRefs:
    - name: oidcClientSecret
      valueFrom:
        secretKeyRef:
          name: tenant-a-oidc-secret
          key: clientSecret
---
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexTenant
metadata:
  name: tenant-a
  namespace: tenant-a # each DexTenant needs its own namespace; see "Multi-tenant routing"
spec:
  issuer: https://tenant-a.example.com/dex
  connectorRef:
    name: tenant-a-connector
```

Mount a single OAuth2 client onto multiple tenants, each getting its own
generated client secret:

```yaml
apiVersion: idp.groeneslot.nl/v1alpha1
kind: DexClient
metadata:
  name: backend-app
spec:
  tenantRefs:
    - name: tenant-a
      namespace: tenant-a
    - name: tenant-b
      namespace: tenant-b
  clientID: backend-app
  redirectURIs:
    - https://backend.example.com/callback
  name: Backend App
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

The Helm chart is generated under `dist/chart` and can install the operator
with or without an in-chart Dex deployment.

Install the operator only and connect it to an externally managed Dex with your
own `DexConnection` resources:

```sh
helm install dex-idp-operator ./dist/chart \
  --namespace dex-idp-operator-system \
  --create-namespace \
  --set manager.image.repository=<some-registry>/dex-idp-operator \
  --set manager.image.tag=<tag>
```

Install the operator with a demo Dex deployment:

```sh
helm install dex-idp-operator ./dist/chart \
  --namespace dex-idp-operator-system \
  --create-namespace \
  --set manager.image.repository=<some-registry>/dex-idp-operator \
  --set manager.image.tag=<tag> \
  --set dex.enabled=true
```

When `dex.enabled=true`, the chart creates a Dex `Deployment`, `Service`,
`ConfigMap`, and a matching `DexConnection` resource by default. The built-in
Dex config uses sqlite storage in `/tmp` and a mock connector, which is suitable
for local development and demos only. For production, either keep `dex.enabled`
disabled and manage Dex separately, or override `dex.config` with production
storage, issuer, and connector settings.

The Makefile also includes convenience Helm targets:

```sh
make helm-deploy IMG=<some-registry>/dex-idp-operator:<tag>
make helm-uninstall
```

**NOTE:** If you change the project, you need to update the Helm Chart
using `kubebuilder edit --plugins=helm/v2-alpha` to sync the latest generated
manifests. Preserve any chart customizations, such as the optional Dex templates
and values, when regenerating.

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

