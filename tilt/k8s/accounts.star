# accounts.star - which all-keys.yaml accounts account-init has to initialise
#
# Derived from the genesis instead of listed by hand, so a generated localnet
# (scripts/localnet/gen-genesis.go) brings its own accounts with it.
# scripts/localnet/check-account-init.sh holds this to the list that was
# written by hand for the default localnet, order included.

def staked_account_names(all_keys, genesis):
    """Names of the all-keys.yaml accounts that are staked actors in genesis.

    Walks the genesis actor lists in order -- applications, then suppliers, then
    gateways -- and names each address by the FIRST all-keys.yaml account that
    carries it, so an address listed under two names is imported once.
    """
    app_state = genesis["app_state"]
    actors = [a["address"] for a in app_state["application"]["applicationList"]]
    actors += [s["operator_address"] for s in app_state["supplier"]["supplierList"]]
    actors += [g["address"] for g in app_state["gateway"]["gatewayList"]]

    first = {}
    for account in all_keys["accounts"]:
        if account["address"] not in first:
            first[account["address"]] = account["name"]

    names = []
    seen = {}
    for address in actors:
        if address in seen:
            continue
        if address not in first:
            fail("staked actor %s has no key in all-keys.yaml" % address)
        seen[address] = True
        names.append(first[address])
    return names
