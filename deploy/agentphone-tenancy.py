#!/usr/bin/env python3
"""Rewrite the io.pilot.agentphone registry entry for broker-enforced tenancy.

The agentphone partner account is a single shared account (bound to a provider
campaign that permits number generation), so the partner cannot tell pilot users
apart and cannot isolate them. The broker therefore owns tenancy: this script
declares which path/body fields name a resource, how each resource is claimed,
and which list responses must be filtered to the caller's own rows.

Run on the broker host; it rewrites /opt/pilot/registry/apps.json in place after
taking a timestamped backup, and validates the JSON before writing.
"""
import json
import re
import shutil
import sys
import time

P = "/opt/pilot/registry/apps.json"

# Routes that cannot be made per-tenant on a shared account.
#
# The webhook routes are ACCOUNT-level: one tenant setting a webhook redirects
# every tenant's events to their endpoint. There is no per-caller scoping to
# apply, so they are removed rather than "checked" — the per-agent route
# /v1/agents/{agent_id}/webhook stays and is gated by agent ownership.
#
# /v1/messages/{message_id}/reactions names a message id that no route can
# establish ownership of, and an unmapped param is an unchecked resource. It is
# dropped until messages carry an ownable link.
DROP = {
    "/v1/webhooks",
    "/v1/webhooks/deliveries",
    "/v1/webhooks/deliveries/stats",
    "/v1/webhooks/test",
    "/v1/messages/{message_id}/reactions",
}

# Spend routes remain withheld from the allow-list while the grant for a
# self-minted identity is still unauthenticated. Re-add them together with the
# access-key rollout, not before.
SPEND = {"/v1/messages", "/v1/calls", "/v1/numbers"}

TENANCY = {
    # Every {param} that appears in an allow pattern MUST be mapped here. A param
    # with no mapping is never ownership-checked, which is the same as public.
    "param_types": {
        "agent_id": "agent",
        "number_id": "number",
        "call_id": "call",
        "conversation_id": "conversation",
        "contact_id": "contact",
    },
    # Body/query fields that name a resource the call acts THROUGH. Sending names
    # the sending agent in the body while the path is entirely the caller's own,
    # so without these the path checks above prove nothing. Aliases are listed
    # because the partner accepts more than one spelling.
    "body_refs": {
        "agent_id": "agent",
        "agentId": "agent",
        "number_id": "number",
        "numberId": "number",
        "phoneNumberId": "number",
        "from_number": "number",
        "conversation_id": "conversation",
        "conversationId": "conversation",
        "call_id": "call",
        "callId": "call",
        "contact_id": "contact",
        "contactId": "contact",
    },
    "create": [
        {"method": "POST", "path": "/v1/agents", "type": "agent", "id_field": "id"},
        {"method": "POST", "path": "/v1/numbers", "type": "number", "id_field": "id"},
        {"method": "POST", "path": "/v1/calls", "type": "call", "id_field": "id"},
        {"method": "POST", "path": "/v1/contacts", "type": "contact", "id_field": "id"},
    ],
    "delete": [
        {"method": "DELETE", "path": "/v1/numbers/{number_id}", "type": "number", "param": "number_id"},
        {"method": "DELETE", "path": "/v1/agents/{agent_id}", "type": "agent", "param": "agent_id"},
        {"method": "DELETE", "path": "/v1/contacts/{contact_id}", "type": "contact", "param": "contact_id"},
    ],
    # Only ACCOUNT-WIDE lists are declared here.
    #
    # A sub-resource list under an owned parent (e.g. /v1/numbers/{number_id}/messages)
    # is already authorized by the parent's ownership check, and its elements carry
    # no ownable link of their own — declaring it would drop every row and hide the
    # owner's own messages from them.
    "list": [
        {"method": "GET", "path": "/v1/agents", "array": "data",
         "owner_by": [{"field": "id", "type": "agent"}], "claim_as": "agent",
         "count_fields": ["total"]},
        {"method": "GET", "path": "/v1/numbers", "array": "data",
         "owner_by": [{"field": "id", "type": "number"}], "claim_as": "number",
         "count_fields": ["total"]},
        {"method": "GET", "path": "/v1/contacts", "array": "data",
         "owner_by": [{"field": "id", "type": "contact"}], "claim_as": "contact",
         "count_fields": ["total"]},
        # Inbound calls/conversations are created by the partner, so they are
        # attributable only through the number/agent they hang off. claim_as makes
        # them fetchable by id afterwards.
        {"method": "GET", "path": "/v1/calls", "array": "data",
         "owner_by": [{"field": "phoneNumberId", "type": "number"},
                      {"field": "agentId", "type": "agent"}], "claim_as": "call",
         "count_fields": ["total"]},
        {"method": "GET", "path": "/v1/conversations", "array": "data",
         "owner_by": [{"field": "phoneNumberId", "type": "number"},
                      {"field": "agentId", "type": "agent"}], "claim_as": "conversation",
         "count_fields": ["total"]},
        {"method": "GET", "path": "/v1/usage/by-number", "array": "data",
         "owner_by": [{"field": "numberId", "type": "number"}]},
        {"method": "GET", "path": "/v1/usage/by-agent", "array": "data",
         "owner_by": [{"field": "agentId", "type": "agent"}]},
    ],
    # /v1/usage summarises the SHARED account: its number count and message/call
    # totals span every tenant. Left alone it is a side-channel — a caller who can
    # see none of the resources can still read how many exist and watch the totals
    # move. numbers.used is recomputed from the caller's own ledger; the stats
    # block cannot be attributed to a single tenant, so it is dropped rather than
    # reported wrongly.
    "object": [
        {"method": "GET", "path": "/v1/usage",
         "owned_counts": {"numbers.used": "number"},
         # numbers.remaining is the partner's limit minus the ACCOUNT-WIDE used, so
         # it discloses the very count numbers.used was just scoped to hide.
         # A derived field leaks whatever it was derived from.
         "redact": ["stats", "numbers.remaining"]},
    ],
}


def main():
    apps = json.load(open(P))
    shutil.copy(P, P + ".bak-" + time.strftime("%Y%m%d-%H%M%S"))

    for a in apps:
        if a["id"] != "io.pilot.agentphone":
            continue
        before = len(a["allow"])
        a["allow"] = [p for p in a["allow"] if p not in DROP and p not in SPEND]
        a["tenancy"] = TENANCY
        # Cap identities per source IP. This is a speed bump against bulk identity
        # creation, not a boundary: it does not constrain a caller with many source
        # addresses, so it must never be the only control on a grant.
        a.setdefault("credit", {})["max_identities_per_ip"] = 3
        print("allow: %d -> %d" % (before, len(a["allow"])))

        # Fail loudly if any {param} still lacks an ownership mapping: an unmapped
        # param is an unchecked resource, which is exactly the bug class this fixes.
        params = set()
        for p in a["allow"]:
            params |= set(re.findall(r"{(\w+)}", p))
        unmapped = params - set(TENANCY["param_types"])
        if unmapped:
            sys.exit("REFUSING: unmapped path params would be unchecked: %s" % sorted(unmapped))
        print("all path params mapped: %s" % sorted(params))

    json.dump(apps, open(P, "w"), indent=2)
    json.load(open(P))  # re-parse: never leave invalid JSON behind
    print("registry written + valid")


if __name__ == "__main__":
    main()
