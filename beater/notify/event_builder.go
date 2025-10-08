package notify

import "github.com/google/uuid"

// MessageType is a small enum
type MessageType int

const (
	RouteValidationError = iota
	RouteValidationCorrection
)

// Builder provides a chainable way to construct Notification values
// with static defaults for all fields except Message, Basic.Type/Details,
// and Detailed.Type.
type Builder struct {
	n Notification
}

// Static defaults
const (
	defaultExpirySeconds = -1
	defaultSeverity      = "critical"
	defaultPriority      = 1
	defaultCategory      = "category-3"
	defaultCategoryLabel = "Routing & Signal Health"
	defaultDetectorApp   = "routebeat"
	defaultVersion       = "2.0"
)

// NewBuilder initializes a Builder with static defaults.
// Only Message fields, Basic.Type/Details, and Detailed.Type should vary.
func NewBuilder(host string) *Builder {
	return &Builder{
		n: Notification{
			Unique: Unique{
				UID:    uuid.NewString(),
				Expiry: defaultExpirySeconds,
			},
			Classification: Classification{
				Severity: defaultSeverity,
				Priority: defaultPriority,
				Category: defaultCategory,
			},
			Origin: Origin{
				Detector: Detector{
					Host: host,
					App:  defaultDetectorApp,
				},
			},
			Context: Context{
				Basic:    make([]Basic, 0, 2),
				Detailed: make([]Detailed, 0, 2),
			},
			Version: defaultVersion,
		},
	}
}

// WithBody sets the Message.Body.
func (b *Builder) WithBody(body string) *Builder {
	b.n.Message.Body = body
	return b
}

// WithSummary sets the Message.Summary.
func (b *Builder) WithSummary(summary string) *Builder {
	b.n.Message.Summary = summary
	return b
}

// WithMessageByType sets the Message.Summary and Message.Body by type
func (b *Builder) WithMessageByType(messageType MessageType) *Builder {
	switch messageType {
	case RouteValidationError:
		b.n.Message.Body = "Route validation transition status from primary to another status"
		b.n.Message.Summary = "[Route Validation Error] Route validation transition from Primary"

	case RouteValidationCorrection:
		b.n.Message.Body = "Route validation transition status to primary from another status"
		b.n.Message.Summary = "[Route Validation Correction] Route validation transition to Primary"
	}

	return b
}

// AddBasic appends a Basic context entry with the given type and details.
func (b *Builder) AddDetails(messageType MessageType, details Details) *Builder {
	var typeVal string

	switch messageType {
	case RouteValidationError:
		typeVal = "Event - Status Changed"

	case RouteValidationCorrection:
		typeVal = "Event - Status Reverted"
	}

	b.n.Context.Basic = append(b.n.Context.Basic, Basic{
		Type:    typeVal,
		Details: details,
	})

	b.n.Context.Detailed = append(b.n.Context.Detailed, Detailed{
		Type: typeVal,
	})

	return b
}

// Build returns the constructed Notification.
func (b *Builder) Build() Notification {
	return b.n
}
