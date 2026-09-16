// Package project defines the Project, the isolation boundary every stored
// record carries. Reach between Projects is an explicit directed link, never
// inheritance: there is no parent Project and no project id meaning "all".
package project
