# Tenant Harbor workflow

FCS derives the private Harbor project from the tenant token. A Terraform
configuration never selects an arbitrary project.

## 1. Create a Robot Account

Apply `examples/harbor/main.tf`, then retrieve the sensitive outputs without
printing them into CI logs:

```sh
terraform output -raw harbor_robot_username > .harbor-user
terraform output -raw harbor_robot_secret > .harbor-secret
chmod 600 .harbor-user .harbor-secret

cat .harbor-secret | docker login harbor.cloud.focusnet.de \
  --username "$(cat .harbor-user)" \
  --password-stdin
```

Use `permission_profile = "push_pull"` only for publishers. Runtime
credentials are created through `fcs_harbor_registry_binding` with a
server-side fixed pull-only profile.

The one-time Robot secret is sensitive, but it is still stored in Terraform
state. Use an encrypted, access-controlled remote backend and rotate the
credential by changing `rotation_trigger`.

## 2. Transfer container images with OCI tools

Terraform deliberately does not transfer image layers. Docker, Podman,
Skopeo, ORAS and Crane can push or pull in the tenant project:

```sh
docker tag example:2026-07 \
  harbor.cloud.focusnet.de/t-<tenant>/apps/example:2026-07
docker push harbor.cloud.focusnet.de/t-<tenant>/apps/example:2026-07
```

After the push, `fcs_harbor_artifact` resolves the tag once and stores the
immutable digest. Consumers should use its `canonical_reference`.

## 3. Deploy containers

See `examples/harbor-kubernetes`. It composes:

- an ephemeral `fcs_cluster_kubeconfig`;
- the official `hashicorp/kubernetes` provider;
- a server-managed `fcs_harbor_registry_binding`; and
- a Kubernetes Deployment using the digest-pinned artifact reference.

The binding writes a pull-only credential directly to the selected tenant
namespace. No registry secret is returned to Terraform state. The same pattern
works with the official Helm provider.

## 4. Prepare and deploy VM disks

`kind = "vm_disk"` expects a CDI-compatible KubeVirt container disk. The OCI
image must contain a bootable raw or QCOW2 disk at `/disk/disk.img`. One common
build pattern is:

```dockerfile
FROM scratch
ADD --chown=107:107 disk.qcow2 /disk/disk.img
```

Build and push it with a standard OCI tool, then register it:

```sh
docker buildx build --platform linux/amd64 \
  -t harbor.cloud.focusnet.de/t-<tenant>/vm-images/example:2026-07 \
  --push .
```

See `examples/harbor-vm`. `fcs_vm.harbor_artifact_id` causes the FCS API to
create an internal pull-only Robot Account, a CDI registry Secret and a
digest-pinned DataVolume. The VM is created only after CDI reports a successful
import. Internal credential material is never exposed through the API or
Terraform state.

Exactly one VM source is allowed:

- `image` for an FCS Golden Image; or
- `harbor_artifact_id` for a registered tenant `vm_disk`.

Destroying `fcs_harbor_artifact` removes only its FCS registration. It never
deletes Harbor manifests or layers, and returns `409 Conflict` while a live VM
still references it.

## Import

Existing FCS registry objects use their FCS UUID:

```sh
terraform import fcs_harbor_robot_account.ci '<uuid>'
terraform import fcs_harbor_artifact.application '<uuid>'
terraform import fcs_harbor_registry_binding.application '<uuid>'
```

Harbor credentials cannot be recovered during import. For an imported Robot
Account, change `rotation_trigger` once to mint a new one-time secret.
