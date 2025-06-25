# Suggested design proposal format

Please use this format to make design proposals. The project will use design proposals for significant new features or broad functional changes.

#### Process

This is not prescriptive, but offered as high-level guidance to help normalize new proposal authoring, discussion, and review.

* Fork the repo and cut a new branch to prepare for a PR.
* Create a new directory under `design/proposals` with a descriptive for your proposal (e.g. automated-widgets).
* Copy this markdown file as a template into your proposal directory, remove this top section, and fill in the sections.
** This is not a writing contest, the important part of this process is to get your great ideas into discussion. We are not a judgemental community :heart:
* Feel free to create an `images` directory in your new proposal directory for diagrams, screenshots, or other things you might like to use link to in your doc.
* Create a PR to begin review and discussion.
** Typical PR guidance applies, and you can mark a PR title WIP if that is useful.

If possible, please consider joining us in [the #kro Slack channel or in our community meeting](../../../README.md#community-participation) to discuss your idea.

As proposals are implemented, include a change to move the proposal from `design/proposals` to `design` in the PR that completes the implementation. :tada:

:bulb: Remove from here up, starting with your title.

# Proposal title

:memo: Please use a descriptive title for your proposal.

## Problem statement

:memo: Short narrative overview of the problem you want to solve.
Include any data, use cases, or other details you'd like to frame the problem.

## Proposal

:memo: Explain your proposal to set the context for more details that follow.

#### Overview

:memo: Now we start getting into the details of your proposal with an overview of the proposed changes.

#### Design details

:memo: Here you can walk through the detailed changes you have in mind.
It is often helpful to break this up either by components or functional areas.

## Other solutions considered

:memo: What other options exist?
Why are they not a suitable or the best choice?

## Scoping

#### What is in scope for this proposal?

:memo: High-level overview of what is in scope for this proposed change.

#### What is not in scope?

:memo: You can protect your ideas from feature creep by plainly stating what you don't intend to solve for here.
You can also use this to indicate related changes which may come in follow-up proposals, making them out of scope for this one.

## Testing strategy

#### Requirements

:memo: What is needed to test this proposed set of changes?
Clusters and other infrastructure, any external resources, and new test fixtures can land here.

#### Test plan

:memo: How will we test this? 
Unit tests, functional tests, e2e testing are all in scope.

## Discussion and notes

:memo: Placeholder for taking notes through discussions, if useful.
