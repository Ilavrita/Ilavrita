package project

import "fmt"

// BotID identifies one server-invoked automation inside one Project.
type BotID string

// Bot is one automation the server runs on a Project's behalf. It presents no
// credential, because the server invokes it rather than authenticating it, so
// this type has no field a secret could be written to.
type Bot struct {
	id      BotID
	project ID
	name    string
	state   ServiceState
}

// BotConfig is what NewBot takes, and the shape a store rehydrates a row
// through. What a bot runs is not part of its identity.
type BotConfig struct {
	ID    BotID
	Name  string
	State ServiceState
}

// NewBot registers an automation in one Project.
func NewBot(owner ID, cfg BotConfig) (Bot, error) {
	if err := ValidateID(owner); err != nil {
		return Bot{}, err
	}

	if err := ValidateBotID(cfg.ID); err != nil {
		return Bot{}, err
	}

	if cfg.Name == "" {
		return Bot{}, fmt.Errorf("%w: %s", ErrMissingServiceName, cfg.ID)
	}

	if !cfg.State.Valid() {
		return Bot{}, fmt.Errorf("%w: %q", ErrUnknownState, string(cfg.State))
	}

	return Bot{id: cfg.ID, project: owner, name: cfg.Name, state: cfg.State}, nil
}

// ID returns the automation's identifier.
func (b Bot) ID() BotID {
	return b.id
}

// Project returns the Project this automation belongs to.
func (b Bot) Project() ID {
	return b.project
}

// Name returns the name an operator suspends this automation by.
func (b Bot) Name() string {
	return b.name
}

// State returns the automation's lifecycle position.
func (b Bot) State() ServiceState {
	return b.state
}

// Principal names this automation as a membership principal. The kind is fixed
// here, so no caller can pair this id with another family's kind.
func (b Bot) Principal() PrincipalRef {
	return PrincipalRef{Kind: PrincipalBot, ID: PrincipalID(b.id)}
}

// TransitionTo returns the automation in its next state.
func (b Bot) TransitionTo(next ServiceState) (Bot, error) {
	state, err := b.state.TransitionTo(next)
	if err != nil {
		return Bot{}, err
	}

	b.state = state

	return b, nil
}

// String renders an automation. It holds no credential, so there is nothing
// here a log line could spell out.
func (b Bot) String() string {
	return fmt.Sprintf("bot %s in %s (%s)", b.id, b.project, b.state)
}

// GoString renders the same text, so %#v does not reach the unexported fields
// and print a shape this type does not otherwise show.
func (b Bot) GoString() string {
	return b.String()
}
