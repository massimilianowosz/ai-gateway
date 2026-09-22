package cli

// defaultKeyBudget is what the CLI offers when creating a key.
//
// Not zero: a key with no budget is refused by the gateway on its first
// request, so offering zero as the default handed out keys that could not be
// used. OAuth subscription traffic does not consume it — a deployment billed
// flat costs nothing — so this is a ceiling on metered spend, not on usage.
const defaultKeyBudget = 10.0
