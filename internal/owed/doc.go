// Package owed answers "anything left?" from records only (SPEC 8 "What a
// session still owes"): for one session, or for every session of a person,
// the open pull requests, the work items held, the cards filed that wait
// for an answer or are approved but not done, and the sessions started that
// still run.
//
// The package owns no table. Each module that keeps such a record registers
// an orchcore.OwedSource and reads only its own records under its own scope
// rules; this brick joins their rows, sorts them and adds what zm cannot
// see, shown as unknown and never left out. It settles nothing and
// changes nothing: closing out is the person's call.
package owed
