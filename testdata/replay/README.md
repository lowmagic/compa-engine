# Replay fixtures

`epistemic/` is synthetic: a worker node's Ceph log directory, its kubelet
log and journal, and a namespace's pods and events, all invented for the
epistemic checks' tests (`engine/check_test.go`, `engine/verdict_test.go`)
and their live benchmark (`engine/check_live_test.go`); nothing comes from
a customer. The tests replay on it the shapes of failure the checks are
for: a listing cited as runtime state, a candidate called an outcome, an
alert rule and threshold no evidence holds, events hours apart told as
one, an error told as a measure, and a qualifier dropped between answers.
One kubelet line ends in a tab and spaces, so a quotation's bytes are
checked as stored.

`references/` holds a knowledge-base solution's page, synthetic too, which
the benchmark's fake reference adapter serves to a `kcs_fetch`, so it is
stored as a `reference_document` item as a deployment's adapter's page
is. It is outside `epistemic/`, the case's artifact root, so no read or
search of the case finds it. Its lines 7–9 are its root cause, about the
environment it describes, not the case's.

The benchmark's second answer, a2, adds to the shapes above:

- 9, precedent as case evidence: the solution's root cause cited as the
  cause of the case's ENOSPC errors;
- 10, absence as evidence of absence: no `ceph-osd.5` log in the Ceph log
  directory's listing told as osd.5 never deployed on the node;
- 11, events in order told as cause and effect: the MGR's start and, 86
  seconds later, the MDS's restart back-off, two events, the first said
  to cause the second;
- 12, a recommendation told as a confirmed fix: restarting the CSI
  plugin's pod said to fix the refused dials, nothing showing it did;
- 13, a consult's hedged answer told as observed: the consulted expert
  says the API's update of the OSD's pod "may be" the Rook operator
  deleting it; a2 says the operator deleted it. The checks are given the
  cited kubelet line, never the conversation or the consult;
- 14, a configuration's head read as the whole: the cluster's proxy
  configuration read to its eighth line, its `noProxy` on the ninth, and
  a2 says it sets none;
- 15, two nodes' captures told as one node's progression: worker-1's and
  worker-2's disk usage, collected two hours apart, told as worker-1's
  `/var/lib/containers` filling from 71% to 96%;
- 16, a request resting on an unsupported causal premise: deleting the
  MDS's pod so it is rescheduled off worker-1, "whose full disk crashed
  its `mds` container", where the cited events show only a back-off
  restarting it, hours before the ENOSPC errors.

A factual claim in a heading is not among them: the engine's statements
leave headings out, so neither check can name or correct one.

Its third answer, a3, is the control: six statements, each supported as
worded by the lines it cites, none seeded, so the checks' false findings
have a denominator of their own. Two of its statements are what a check
may still flag: one citing only the listing, and the solution's root
cause given as the solution's.

Its fourth answer, a4, the reviewer alone reviews, without and then with
the ranked evidence it does not cite (`/review --ranked`): shape 17, the
eviction manager's ranking of the CSI plugin's pod, which the
conversation read and a1 and a2 cited, dismissed as never logged, beside
two statements the lines a4 cites support. a4 cites only the refused
dial and the OSD's shutdown, so only the ranked evidence quotes the
ranking; a finding resting on its lines names the omission.

After a3 the editor checks a customer draft written from a1, a2 and a3
that cites nothing, as drafts in live use do: four statements the lines
those answers cite hold, and a fifth, an alert's firing, that nothing
holds. Given the items the answers cite, the editor should cite the
lines for the four rather than anchor them for a revision that reads no
evidence, and anchor or flag the fifth, never cite it.

They are kept apart from `testdata/artifacts`, the stub case's artifact
directory, so the attachments other tests see are unchanged.
