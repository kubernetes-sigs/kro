---
id: roadmap
title: Roadmap & Active Development Areas
sidebar_position: 101
---
_Last updated: 2025-11-12 15:10:03Z_

Kro’s roadmap highlights **active initiatives**, **planned features**, and **proposal areas open for feedback**.

This file is auto-generated from [GitHub issues](https://github.com/kubernetes-sigs/kro/issues) labeled with `roadmap-item`, `kind/feature`, `priority/mega-issue`, `needs/community-input`, or `area/*`.

---

## In Progress (Major initiatives)

_No in-progress initiatives currently listed._

---

## Planned Enhancements

### Area: General
- [Support GVK reconciliation for a certain time after RGD is deleted](https://github.com/kubernetes-sigs/kro/issues/789)
- [Allow default property definitions in the spec to reference other property values.](https://github.com/kubernetes-sigs/kro/issues/781)
- [ResourceGraphDefinition -> CRD ownership](https://github.com/kubernetes-sigs/kro/issues/745)
- [Separate out simple schema into its own package](https://github.com/kubernetes-sigs/kro/issues/720)
- [Support downstream propagation of labels/annotations](https://github.com/kubernetes-sigs/kro/issues/652)
- [Ideation: Integrating with resources outside of the scope of an RGD](https://github.com/kubernetes-sigs/kro/issues/638)
- [allow new preserveDefaultPrinterColumns field in RGDs to augment existing additionalPrinterColumns field](https://github.com/kubernetes-sigs/kro/issues/629) — Assigned to @yashpal2104
- [Enhanced resource level error reporting to the client](https://github.com/kubernetes-sigs/kro/issues/609)
- [Determine if cross-namespace, or namespace-pinned resources in an RGD are allowed](https://github.com/kubernetes-sigs/kro/issues/513)
- [Design and naming review before committing to stable version](https://github.com/kubernetes-sigs/kro/issues/511)
- [Feature flags](https://github.com/kubernetes-sigs/kro/issues/510)
- [Update Versions](https://github.com/kubernetes-sigs/kro/issues/495)
- [Allow ResourceGraphDefinition authors to maintain serveral versions](https://github.com/kubernetes-sigs/kro/issues/482)
- [Support test fixtures for creating ResourceGraphDefinitions](https://github.com/kubernetes-sigs/kro/issues/418)
- [Enable OpenSSF Scorecard](https://github.com/kubernetes-sigs/kro/issues/390)
- [Simplify Developer Tooling Setup in the repo](https://github.com/kubernetes-sigs/kro/issues/372)
- [Add support for custom functions](https://github.com/kubernetes-sigs/kro/issues/361)
- [Skip Kind verification for resources that rely on CRDs that are not yet present in the cluster](https://github.com/kubernetes-sigs/kro/issues/330)
- [Support for drift-detection / re-reconciliation?](https://github.com/kubernetes-sigs/kro/issues/323) — Target: 0.6
- [Rolling apply new resource group definition](https://github.com/kubernetes-sigs/kro/issues/274)
- [Add support for weight based queue management in dynamiccontroller](https://github.com/kubernetes-sigs/kro/issues/198)
- [expose CEL execution prometheus metrics](https://github.com/kubernetes-sigs/kro/issues/195) — Assigned to @TusharMohapatra07
- [validate CEL expressions against OpenAPI schemas at compile time](https://github.com/kubernetes-sigs/kro/issues/194)
- [implement CEL runtime cost budget in controller](https://github.com/kubernetes-sigs/kro/issues/191)
- [expose CEL expression execution cost in ResourceGroup status](https://github.com/kubernetes-sigs/kro/issues/190) — Assigned to @govindup63
- [track ResourceGroup changes and opt-in updates](https://github.com/kubernetes-sigs/kro/issues/188) — Assigned to @a-hilaly
- [track and garbage collect orphaned resources](https://github.com/kubernetes-sigs/kro/issues/187)
- [detect and handle CRD breaking changes](https://github.com/kubernetes-sigs/kro/issues/186) — Assigned to @a-hilaly — Target: 0.7
- [propagate RGI labels to managed resources](https://github.com/kubernetes-sigs/kro/issues/184) — Assigned to @RinkiyaKeDad
### Area: api
- [Support for handling resource life-cycle modifiers](https://github.com/kubernetes-sigs/kro/issues/542) — Assigned to @barney-s — Target: 0.7
### Area: cel
- [CEL versioning](https://github.com/kubernetes-sigs/kro/issues/643) — Assigned to @jakobmoellerdev
### Area: cli
- [Native support for inline file references](https://github.com/kubernetes-sigs/kro/issues/570)
### Area: runtime
- [Support referencing instance status fields in RGD resources](https://github.com/kubernetes-sigs/kro/issues/762)
### Area: simpleschema
- [Dynamic default values in schema](https://github.com/kubernetes-sigs/kro/issues/579)
### Area: testing
- [Continuous performance testing](https://github.com/kubernetes-sigs/kro/issues/610) — Assigned to @a-hilaly

---

## Proposal Discussions & Community Input

_No open proposal discussions at the moment._


---

## How to Contribute
- Read the [security section of CONTRIBUTING](https://github.com/kubernetes-sigs/kro/blob/main/CONTRIBUTING.md#security)
- Open issues labeled \`needs/community-input\` or \`kind/proposal\` to propose design ideas
- Contribute PRs that reference roadmap items

---

## Community Participation

Development and discussion is coordinated in the [**Kubernetes Slack**](https://communityinviter.com/apps/kubernetes/community) in the [**#kro**](https://kubernetes.slack.com/archives/C081TMY9D6Y) channel.

**Please join our community meeting**:

- **Cadence:** Every other Wednesday
- **Time:** 9:00 AM PT (Pacific Time) — [Convert to your local timezone](https://dateful.com/time-zone-converter?t=9%3A00&tz=PT%20%28Pacific%20Time%29)
- **Agenda:** [Public Google Doc](https://docs.google.com/document/d/1GqeHcBlOw6ozo-qS4TLdXSi5qUn88QU6dwdq0GvxRz4/edit?tab=t.0)
- **Join:** [Zoom Meeting](https://us06web.zoom.us/j/85388697226?pwd=9Xxz1F0FcNUq8zFGrsRqkHMhFZTpuj.1)
- **Recordings:** [YouTube Channel](https://www.youtube.com/channel/UCUlcI3NYq9ehl5wsdfbJzSA)

---

_This page is automatically generated weekly by CI._

