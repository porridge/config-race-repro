# Reproduce a race in helm operator plugins

## What is this?

This project provides a minimal reproduction case for a bug found in StackRox operator
that uses the helm-operator-plugins library.

## How do I use this?

**Note** that you need an environment where the program can talk to a Kubernetes server.
(It uses the same mechanism as `kubectl` to discover the kube config.)

Just run `go run -race .` in this directory.
You should see a report of a few data races found. See [example output](#example-output).

Use the flags (see `-help`) to:
- bump the parallelism,
- switch to an implementation which does not have the bug,
- turn on a fix in the helm-operator-plugins implementation

## What is the actual problem?

First, let me describe how the pieces come together in an operator.
1. The `helm-operator-plugins` library uses `helm` library's `kube.Client` to interact wih the cluster
2. In order to [create the helm kube client](https://github.com/operator-framework/helm-operator-plugins/blob/a307065f8d96cb3bb805c75f90d6aea43fd32709/pkg/client/actionconfig.go#L71)
   the library needs to pass it an implementation of `k8s.io/cli-runtime/pkg/genericclioptions.RESTClientGetter`
3. The `helm-operator-plugins` library has its own implementation of `RESTClientGetter`, namely [`restClientGetter`](https://github.com/operator-framework/helm-operator-plugins/blob/a307065f8d96cb3bb805c75f90d6aea43fd32709/pkg/client/restclientgetter.go#L41)
4. When a `restClientGetter` is [created](https://github.com/operator-framework/helm-operator-plugins/blob/a307065f8d96cb3bb805c75f90d6aea43fd32709/pkg/client/actionconfig.go#L59)
   it is passed a pointer to `rest.Config` that is [by default supplied](https://github.com/operator-framework/helm-operator-plugins/blob/a307065f8d96cb3bb805c75f90d6aea43fd32709/pkg/reconciler/reconciler.go#L877) by the `controller-runtime`
   Manager object when the parent `helm-operator-plugins` `Reconciler` object is created.
5. **Note** that `restClientGetter` simply [returns](https://github.com/operator-framework/helm-operator-plugins/blob/a307065f8d96cb3bb805c75f90d6aea43fd32709/pkg/client/restclientgetter.go#L51)
   a pointer to the `rest.Config` it had been provided.
6. Now, in order to query and manipulate Kubernetes resources, the `helm` library's `kube.Client` actually delegates most of its 
   work to the [`k8s.io/cli-runtime/pkg/resource`](https://github.com/kubernetes/cli-runtime/tree/master/pkg/resource) library.
   The central part of this library is the
   [`Builder` type](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/builder.go#L54), which provides
   facilities to iterate over resources on a cluster or defined in a YAML file.
7. Individual resources are encapsulated in an [`Info` type](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/visitor.go#L63)
   which among other things contains a `RESTClient` *specific to the GroupVersion of the resource at hand*.
8. In our use case, the `Client` for those `Info` objects is [populated by a `mapper` type's `infoForData` method](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/mapper.go#L76-L80).
   The `clientFn()` there which produces a client is actually the [getClient() method](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/builder.go#L944) of the `Builder` type.
9. In our use case, the heavy lifting is done by the `unstructuredClientForGroupVersion` function, but its sister
   `clientForGroupVersion` has similar issues. These are part of the [ClientConfigFunc](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/interfaces.go#L33) type
   and can be tacked onto any function that returns a `rest.Config` pointer and an `error`.
10. What [this `unstructuredClientForGroupVersion` function](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/client.go#L44) does, is:
    1. [retrieve the actual client config object](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/client.go#L27) by calling the receiver function.
       Note that in our case this is the [ToRESTConfig method](https://github.com/operator-framework/helm-operator-plugins/blob/a307065f8d96cb3bb805c75f90d6aea43fd32709/pkg/client/restclientgetter.go#L51)
       of the `helm-operator-plugins` `restClientGetter` type that **returns a pointer to the single shared `rest.Config` struct**
    2. [mutates the client config](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/client.go#L31-L39)
       to set various fields specific to the `Kind` for which it is preparing a client
    3. [passes the client config](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/client.go#L41)
       to a client constructor. That constructor then copies various fields from the config
       into a newly constructed `RESTClient`. Note that if any other goroutine happens to mutate the shared `rest.Config` between steps 2 and 3,
       this will result in a corrupted `RESTClient`. This is exactly how [we've noticed the bug](#how-youve-noticed-the-bug).

## How you've noticed the bug?

The StackRox operator controller process runs [two](https://github.com/stackrox/stackrox/blob/d785cefc453d3be767c59dbd5361e08c840a7101/operator/main.go#L111-L117) `helm-operator-plugins` `Reconciler`s. 

When they were both busy, they would step on each other's ~~toes~~ shared `Config` struct.
In particular, during an end-to-end test, when one of the reconcilers would be deleting a Helm release, sometimes this would lead to `DELETE`
requests being sent for franken-resources such as:

```json
{
  "resource": "podsecuritypolicies",
  "name": "stackrox-kuttl-test-neutral-ladybug-central",
  "apiGroup": "rbac.authorization.k8s.io",
  "apiVersion": "v1"
}
```

Note there is no type `PodSecurityPolicy` in the `rbac.authorization.k8s.io` APIGroup. A correct request would look like this:

```json
{
  "resource": "podsecuritypolicies",
  "name": "stackrox-kuttl-test-prime-penguin-central",
  "apiGroup": "policy",
  "apiVersion": "v1beta1"
}
```

Such nonsensical requests were rejected by the Kubernetes API server with a `404`. These would in turn be silently
ignored by the `helm` library code, because it would interpret the error as "that particular resource was already deleted"
rather than "such type does not exist". Typically, in case of a namespaced resource, we would not even notice, because
Kubernetes garbage collector would later get rid of it, thanks to an owner reference.

However, if the given resource was a cluster-scoped one (our parent custom resource is namespaced) garbage collection would not kick in,
and if the name was not unique (which is the case for one of our operands), a subsequent end-to-end test would clash with it.
This would lead to a flake.

This was quite puzzling, because we would see complaints about clashing resources, while the controller logs clearly indicated
that the resource was explicitly deleted during the previous test.

First, we added an assertion between the tests to make sure that all resources were gone.
This would catch the bug slightly earlier, and even in the case of uniquely named resources.

Then, thanks to the Kubernetes API server audit log, we found that the cause is a corrupted `DELETE` request.

Then, thanks to some [good old printf-based debugging](https://github.com/porridge/helm/commit/c5992273573d51b9741c3f784b29a7e98e780165)
we managed to establish that the corruption is happening somewhere within [this](https://github.com/helm/helm/blob/663a896f4a815053445eec4153677ddc24a0a361/pkg/kube/client.go#L298-L309) `perform()` call.

Finally, after digging into the code and understanding that the GroupVersion is actually stored in the Client,
[a little earlier in the code path](https://github.com/helm/helm/blob/663a896f4a815053445eec4153677ddc24a0a361/pkg/action/uninstall.go#L214)
we found the root cause.

The [full picture](pics/bug.png) was somewhat complicated, so we created this minimal repro project.

<img src="pics/bug.png?raw=true" width="1000">

## Root cause

As described in detail above, the root cause seems to be an impedance mismatch between the `k8s.io/cli-runtime` library and its use by `helm-operator-plugins`.

The `k8s.io/cli-runtime/pkg/resource.Builder` type, and in particular the [ClientConfigFunc](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/interfaces.go#L33) type
seem to assume that they are free to assume ownership of the `rest.Config` returned by the
[supplied](https://github.com/kubernetes/cli-runtime/blob/ba42b4014925e8e17241197c8b3a1ba2e0795c97/pkg/resource/builder.go#L207)
`RESTClientGetter`'s `ToRESTConfig` function.

There is no mention of this assumption in the [exported interface documentation](https://github.com/kubernetes/cli-runtime/blob/ba42b4014925e8e17241197c8b3a1ba2e0795c97/pkg/genericclioptions/config_flags.go#L64-L65),
and the [comment for](https://github.com/kubernetes/cli-runtime/blob/ba42b4014925e8e17241197c8b3a1ba2e0795c97/pkg/resource/builder.go#L60-L61) the (private) field in which the function is stored in the `Builder` is somewhat confused.

At the same time the [implementation of `ToRESTConfig` in the `ConfigFlags` type](https://github.com/kubernetes/cli-runtime/blob/ba42b4014925e8e17241197c8b3a1ba2e0795c97/pkg/genericclioptions/config_flags.go#L130)
is careful to return a pointer to a new `rest.Config` on each call, but this is also not obvious without reading its source.

The `helm-operator-plugins` library assumed it is fine to return a pointer to a shared instance. This happens to work OK
as long as there is only a single `Reconciler` at play. This because a single `Reconciler` only happens works on a single GroupVersion at a time.
This is because even though some operations happen in parallel, there is explicit
[synchronization](https://github.com/helm/helm/blob/9fe4f2ea72ef795d250f716c3d4146127c1f8040/pkg/kube/client.go#L413-L414) in `helm` library,
at just the right level.
However, helm could get rid of this synchronization at any point, and it is in principle legal to run multiple `Reconcilers` in a single controller process.

## Potential fixes

The fix seems to be to "clone" the `rest.Config` and have the methods of  
the [ClientConfigFunc](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/interfaces.go#L33)
type mutate a copy rather than the shared object. (Trying to protect it with a mutex instead seems hard an unnecessarily complex).

There are at least three places where this could happen:
- at `Reconciler` creation time, in order to pass a different `rest.Config` to each `Reconciler`. This would work with the current
  code thanks to the synchronization mentioned in the [previous section](#root-cause). However, it seems unwise to count on this going forward.
- in the `ToRESTConfig()` method of the `restClientGetter` type in `helm-operator-plugins`.  
  This would (and in fact does, see the `-clone-config` flag of the repro program) fix the race for this use case, but not help potential
  other users of the `Builder` who are affected.
- in [all those methods](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/client.go#L26-L69)
  of `ClientConfigFunc` which call the receiver. This has the benefit of helping all users of the `k8s.io/cli-runtime` library - there
  are likely *many more*, some of them potentially having a similar problem. However since these methods [are chain-called](https://github.com/kubernetes/cli-runtime/blob/9139cfdcab39ff4b6b1c6a42af95855c2b5e57ed/pkg/resource/builder.go#L954-L956)
  this might be tricky to do without needlessly double-copying the config. 

## Example output

```
[mowsiany@mowsiany config-race-repro]$ go run -race . -use-helm-operator-getter=false
using k8s CLI runtime client getter directly
[mowsiany@mowsiany config-race-repro]$ go run -race . -use-helm-operator-getter=true -clone-config
using helm-operator-plugins rest client getter
[mowsiany@mowsiany config-race-repro]$ go run -race . -use-helm-operator-getter=true
using helm-operator-plugins rest client getter
==================
WARNING: DATA RACE
Write at 0x00c0004f9460 by goroutine 22:
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:49 +0x15b
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Previous write at 0x00c0004f9460 by goroutine 23:
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:49 +0x15b
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Goroutine 22 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228

Goroutine 23 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228
==================
==================
WARNING: DATA RACE
Write at 0x00c0004f9450 by goroutine 22:
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:54 +0x25d
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Previous write at 0x00c0004f9450 by goroutine 23:
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:54 +0x25d
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Goroutine 22 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228

Goroutine 23 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228
==================
==================
WARNING: DATA RACE
Read at 0x00c000520ca0 by goroutine 22:
  runtime.racereadrange()
      <autogenerated>:1 +0x1b
  k8s.io/client-go/rest.RESTClientFor()
      /home/mowsiany/go/pkg/mod/k8s.io/client-go@v0.23.1/rest/config.go:320 +0x6a
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:57 +0x297
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Previous write at 0x00c000520ca0 by goroutine 23:
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:44 +0x75
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Goroutine 22 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228

Goroutine 23 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228
==================
==================
WARNING: DATA RACE
Read at 0x00c00044a0e0 by goroutine 23:
  runtime.racereadrange()
      <autogenerated>:1 +0x1b
  k8s.io/client-go/rest.RESTClientForConfigAndClient()
      /home/mowsiany/go/pkg/mod/k8s.io/client-go@v0.23.1/rest/config.go:347 +0xa4
  k8s.io/client-go/rest.RESTClientFor()
      /home/mowsiany/go/pkg/mod/k8s.io/client-go@v0.23.1/rest/config.go:330 +0xb0
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:57 +0x297
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Previous write at 0x00c00044a0e0 by goroutine 22:
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:44 +0x75
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Goroutine 23 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228

Goroutine 22 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228
==================
==================
WARNING: DATA RACE
Read at 0x00c00044a160 by goroutine 23:
  runtime.racereadrange()
      <autogenerated>:1 +0x1b
  k8s.io/client-go/rest.RESTClientFor()
      /home/mowsiany/go/pkg/mod/k8s.io/client-go@v0.23.1/rest/config.go:330 +0xb0
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:57 +0x297
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Previous write at 0x00c00044a160 by goroutine 22:
  k8s.io/cli-runtime/pkg/resource.ClientConfigFunc.unstructuredClientForGroupVersion()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/client.go:44 +0x75
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:937 +0x1e9
  k8s.io/cli-runtime/pkg/resource.(*Builder).getClient-fm()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/builder.go:925 +0x84
  k8s.io/cli-runtime/pkg/resource.(*mapper).infoForData()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/mapper.go:71 +0x87b
  k8s.io/cli-runtime/pkg/resource.(*StreamVisitor).Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:572 +0x349
  k8s.io/cli-runtime/pkg/resource.EagerVisitorList.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:209 +0x1b9
  k8s.io/cli-runtime/pkg/resource.(*EagerVisitorList).Visit()
      <autogenerated>:1 +0x6a
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.FlattenListVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:387 +0x112
  k8s.io/cli-runtime/pkg/resource.(*FlattenListVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.ContinueOnErrorVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:350 +0x161
  k8s.io/cli-runtime/pkg/resource.(*ContinueOnErrorVisitor).Visit()
      <autogenerated>:1 +0x66
  k8s.io/cli-runtime/pkg/resource.DecoratedVisitor.Visit()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/visitor.go:322 +0x10d
  k8s.io/cli-runtime/pkg/resource.(*DecoratedVisitor).Visit()
      <autogenerated>:1 +0xa6
  k8s.io/cli-runtime/pkg/resource.(*Result).Infos()
      /home/mowsiany/go/pkg/mod/k8s.io/cli-runtime@v0.23.1/pkg/resource/result.go:122 +0x161
  main.iterateUsingResourceBuilder()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:56 +0x3b5
  main.main.func1()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:27 +0x73

Goroutine 23 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228

Goroutine 22 (running) created at:
  main.main()
      /home/mowsiany/go/src/github.com/porridge/config-race-repro/repro.go:25 +0x228
==================
Found 5 data race(s)
exit status 66
[mowsiany@mowsiany config-race-repro]$ 
```
