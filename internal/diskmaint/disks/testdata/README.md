# Kubelet stats summary golden

`control-summary.json` is the kubelet `/stats/summary` document the control disk
reads: `ParseNodeStats` takes its share from `node.fs` and `TopConsumers` takes
the biggest consumers from its pods, so one document drives both tests.

The node and pod identities are the control cluster's own, captured 2026-11-05
in `internal/sweeprunner/testdata/live-cluster/capacity-listing.json`: node
`colima-gt-control`, the Forgejo, forgejo-trial and CI runner pods, and the
kube-system and registry-forward pods that share the node. The runner's
`dind-data` volume is claim `forgejo-runner-dind`
(`deploy/k8s/forgejo-runner`), and its 61 GB store and the node's 91% reading
are the incident's figures (`docs/guides/control-cluster-disk.md`).

The document is trimmed to the fields those two readers take: `node.fs`, and
each pod's `podRef`, `ephemeral-storage` and claim-backed volumes. A pod's
`ephemeral-storage` carries the node filesystem's capacity and free bytes with
that pod's own usage, as the kubelet reports it. The remaining byte and inode
counts are the fixture's, sized to keep the document consistent; they are not a
live capture of those pods.

To refresh from a live node, read the summary through the control context and
trim it to the fields above, then update the consumer string the tests expect:

    kubectl --kubeconfig <kubeconfig> --context <context> get --raw \
      /api/v1/nodes/colima-gt-control/proxy/stats/summary
