package orchapi

// ActionHand is a card action that hands a brief to the approver's own
// agent (the Mezze app's "Hand to my agent"): for an approver with no machine that
// takes tasks, the press preapproves an agent on their own machine to work
// from Brief (Agent, at most MaxActionBrief bytes) until the card's verify
// check passes. zm runs nothing for it; it is checked and frozen like the
// other kinds (SPEC 16 "Card judge"). Not generated: zm's own kind.
const ActionHand = "hand"
