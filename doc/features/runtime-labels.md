# Runtime Labels

In PD-disaggregated inference a Pod's serving role can change while the Pod keeps running: an
external switch service moves an instance from prefill to decode (or back) without a restart.
RBG supports this by drawing a line between the Pod metadata it owns and the metadata it leaves
to you. This page states that ownership contract.

## Two identities on one Pod

| | Topology identity | Serving identity |
|---|---|---|
| Example key | `rbg.workloads.x-k8s.io/role-name` | `inference.workload.x-k8s.io/pd-role` |
| Owner | RBG | You / your switch service |
| Mutable at runtime | No | Yes |
| Purpose | Scheduling, affinity, service discovery, revision tracking | Which role the instance is currently serving |

Topology identity is fixed for the lifetime of the Pod. Every key under
`rbg.workloads.x-k8s.io/` belongs to RBG — see
[Labels, Annotations and Environment Variables](../reference/variables.md) for the full list.
Do not write to those keys from an external controller.

Serving identity is a key you choose, outside every RBG-managed prefix. RBG never reverts it.

## Declaring a runtime key

Declare the key in the role's **Pod template** metadata:

```yaml
apiVersion: workloads.x-k8s.io/v1alpha2
kind: RoleBasedGroup
metadata:
  name: pd-pool
spec:
  roles:
    - name: pdmix
      replicas: 4
      rolloutStrategy:
        type: RollingUpdate
        rollingUpdate:
          type: InPlaceIfPossible
      standalonePattern:
        template:
          metadata:
            labels:
              inference.workload.x-k8s.io/pd-role: unassigned
          spec:
            containers:
              - name: sglang
                image: lmsysorg/sglang:v0.5.9-cu124
```

The template value is only the **initial** value. From the moment the Pod exists, the live value
belongs to whoever patches it.

> `spec.roles[].labels` is not the right place. Those labels are applied to the RoleInstanceSet
> and RoleInstance objects, not to Pods. Only the Pod template's metadata reaches Pod metadata.

## Switching the role at runtime

Patch the live Pod. A switch service should send a minimal patch touching only its own key,
never the whole object:

```bash
kubectl patch pod pd-pool-pdmix-0 --type=merge \
  -p '{"metadata":{"labels":{"inference.workload.x-k8s.io/pd-role":"prefill"}}}'
```

Annotations behave identically:

```bash
kubectl patch pod pd-pool-pdmix-0 --type=merge \
  -p '{"metadata":{"annotations":{"inference.workload.x-k8s.io/pd-state":"draining"}}}'
```

The Pod is not recreated. Its UID, IP and node assignment are unchanged, so the switch does not
interrupt the running container.

## Switch flow

The full sequence is owned by an external switch service, not by RBG:

1. Set the Pod's runtime label to `unassigned` (or remove it) so the router stops sending new traffic.
2. Drain in-flight requests and transfer KV cache.
3. Call the engine's switch API. For SGLang this is `POST /pd_role_switch` with
   `{"new_role": "decode"}`, exposed by launching with `--enable-pd-role-switch`
   ([upstream PR](https://github.com/sgl-project/sglang/pull/28403)).
4. Wait until the engine reports the new role as ready.
5. Patch the runtime label to the new role; the router resumes routing by label.

RBG takes part in none of these steps. It does not interpret, validate or manage the label value.

Draining before the flip is a correctness requirement rather than an optimisation: an in-flight
decode request's KV is owned by an upstream prefill.

## Why new and recreated Pods are safe

A recreated Pod gets the template's initial value, not the value the switch service last wrote.
The recommended convention makes that safe by default:

- Declare `unassigned` in the template.
- Have the router select only Pods labelled `prefill` or `decode`.
- Have the switch service assign a role only after the engine has warmed up.

A new Pod then takes no traffic until it is explicitly assigned, so neither a scale-up nor a Pod
recreation can accidentally serve in the wrong role.

`unassigned` is a **routing** state, not an engine mode. The engine still launches with a concrete
`--disaggregation-mode`; the label only decides whether the router sends it traffic.

## Why the value survives

Three properties of the reconcile path together produce the guarantee:

1. Pods are rendered from the template **once**, at creation. Later reconciles do not re-apply
   template metadata to a live Pod.
2. An in-place update builds its metadata patch by diffing the **old and new ControllerRevision
   templates**, then applies that patch to the live Pod. A key whose template value is unchanged
   between the two revisions produces no patch entry, so the live value is left alone.
3. RBG does not compare a live Pod's metadata against the template to correct drift.

## The boundary

The guarantee holds only while the key's **template** value stays unchanged. If a rollout changes
that same key in the template, the revision diff contains it and the new template value overwrites
the live one — RBG treats an explicit template change as an instruction.

So declare the key once with a placeholder such as `unassigned`, and do not edit it in the template
afterwards. If you do change the placeholder, expect live Pods to be reset to it at the next rollout.

## Verification

The envtest suite `test/envtest/testcase/runtime_label/` pins this contract. It asserts that an
externally patched label and annotation survive both steady-state reconciliation and a completed
in-place update, and that the Pod UID is unchanged — so preservation by recreation cannot pass.

See [Update Strategy](update-strategy.md) for configuring in-place updates, and
[PD-disaggregated inference with a runtime-switchable pool](../../examples/inference/pd-runtime-switch.yaml)
for a runnable four-role example. That example wires routing to the label with two Services whose
selectors include `inference.workload.x-k8s.io/pd-role`, so relabelling a Pod moves it between the
prefill and decode backend sets without RBG involvement.