#!/usr/bin/env bash
set -euo pipefail

echo "Kro Roadmap Generator"

REPO="kubernetes-sigs/kro"
OUT_FILE="website/docs/roadmap/roadmap.md"
TOKEN="${GITHUB_TOKEN:-}"

# === Validate dependencies ===
if [[ -z "$TOKEN" ]]; then
  echo "Error: GITHUB_TOKEN not set"
  exit 1
fi

if ! command -v jq &>/dev/null; then
  echo "Error: jq not installed"
  exit 1
fi

# === Fetch issues ===
echo "Fetching roadmap-related issues..."
response=$(curl -s -H "Authorization: token $TOKEN" \
  "https://api.github.com/repos/${REPO}/issues?state=open&per_page=100")

# Validate response
if ! echo "$response" | jq empty >/dev/null 2>&1; then
  echo "Error: Invalid GitHub API response"
  echo "$response" | head -n 10
  exit 1
fi

# === Filter roadmap-related issues ===
filtered=$(echo "$response" | jq '[.[] | select(.labels and any(.labels[]?.name;
  . == "roadmap-item" or
  . == "priority/mega-issue" or
  . == "kind/feature" or
  . == "kind/proposal" or
  . == "needs/community-input" or
  . == "discussion" or
  startswith("area/") ))]')

count=$(echo "$filtered" | jq 'length')
echo "Found $count roadmap-related issues"

in_progress=$(echo "$filtered" | jq '[.[] | select(any(.labels[]?.name; . == "priority/mega-issue"))]')
planned=$(echo "$filtered" | jq '[.[] | select(any(.labels[]?.name; . == "kind/feature"))]')
community=$(echo "$filtered" | jq '[.[] | select(any(.labels[]?.name; . == "needs/community-input" or . == "kind/proposal" or . == "discussion"))]')

render_grouped() {
  local input_json="$1"
  echo "$input_json" | jq -r '
    map(
      {
        area: ( ([.labels[]?.name | select(startswith("area/"))] | map(sub("area/";"")) | first) // "General" ),
        title: .title,
        url: .html_url,
        assignee: (.assignee.login // ""),
        milestone: (.milestone.title // "")
      }
    )
    | group_by(.area)
    | sort_by(.[0].area)
    | .[]
    | ("### " + "Area: " + (.[0].area)),
      ( .[] | "- [" + .title + "](" + .url + ")" +
        (if .assignee != "" then " — Assigned to @" + .assignee else "" end) +
        (if .milestone != "" then " — Target: " + .milestone else "" end) )
  '
}

# === Start writing output ===
{
cat <<EOF
---
id: roadmap
title: Roadmap & Active Development Areas
sidebar_position: 101
---
_Last updated: $(date -u +"%Y-%m-%d %H:%M:%SZ")_

Kro’s roadmap highlights **active initiatives**, **planned features**, and **proposal areas open for feedback**.

This file is auto-generated from [GitHub issues](https://github.com/${REPO}/issues) labeled with \`roadmap-item\`, \`kind/feature\`, \`priority/mega-issue\`, \`needs/community-input\`, or \`area/*\`.

---

## In Progress (Major initiatives)

EOF

if [[ $(echo "$in_progress" | jq 'length') -gt 0 ]]; then
  render_grouped "$in_progress"
else
  echo "_No in-progress initiatives currently listed._"
fi

cat <<'EOF'

---

## Planned Enhancements

EOF

if [[ $(echo "$planned" | jq 'length') -gt 0 ]]; then
  render_grouped "$planned"
else
  echo "_No planned enhancements listed._"
fi

cat <<'EOF'

---

## Proposal Discussions & Community Input

EOF

if [[ $(echo "$community" | jq 'length') -gt 0 ]]; then
  render_grouped "$community"
else
  echo "_No open proposal discussions at the moment._"
fi

comm_coord=$(echo "$filtered" | jq '[.[] | select(any(.labels[]?.name; startswith("committee/") or startswith("area/community-")))]')
if [[ $(echo "$comm_coord" | jq 'length') -gt 0 ]]; then
  cat <<'EOF'

---

## Community & Governance

EOF
  echo "$comm_coord" | jq -r '
    map(
      {
        group: ( ([.labels[]?.name | select(startswith("committee/"))] | map(sub("committee/";"")) | first)
                 // ([.labels[]?.name | select(startswith("area/community-"))] | map(sub("area/community-";"")) | first)
                 // "General"),
        title: .title,
        url: .html_url
      }
    )
    | group_by(.group)
    | sort_by(.[0].group)
    | .[]
    | ("### " + .[0].group),
      ( .[] | "- [" + .title + "](" + .url + ") — *Community Coordination*" )
  '
fi

# === Updated Community Participation Section with Links ===
cat <<'EOF'


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

EOF
} > "$OUT_FILE"

echo "Roadmap written to $OUT_FILE"
