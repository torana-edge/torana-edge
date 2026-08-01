package plugin

// Recursive outbound-policy enforcement for StreamEvents.
//
// The SDK owns the inventory.  This host-side walk deliberately consumes that
// inventory instead of maintaining a second table of stream fields: adding a
// field to the v2 proto without a policy makes outboundpolicy.Validate fail at
// startup, and this walk then has one authoritative answer for every field.
//
// Stream events are different from ordinary protobuf replacement messages in
// one important way: a plugin is allowed to buffer fragments and later emit a
// semantically equivalent assembled event.  The caller therefore invokes this
// transaction only at a completed accepted scope, never for a single emitted
// fragment.  Event-list cardinality/order/action changes require the additive
// topology grant; recursively changed semantic fields require their own
// sections as well.  Host-owned fields reject regardless of grants.  Bound
// signatures are intentionally delegated to verifyStream{Prefix}, which owns
// their whole-scope correlation rule.

import (
	"bytes"
	"fmt"

	"github.com/torana-edge/torana-plugin-sdk/outboundpolicy"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// verifyStreamPolicy enforces every non-signature stream field policy over a
// completed transaction.  It intentionally has no fast path: host-owned facts
// and unknown protobuf bytes must be checked even for a plugin holding every
// write grant.
func verifyStreamPolicy(accepted, returned []*pbv2.StreamEvent, canWrite func(string) bool) error {
	if canWrite == nil {
		canWrite = func(string) bool { return false }
	}
	d := streamPolicyDiff{canWrite: canWrite}
	return d.events(accepted, returned, "events")
}

type streamPolicyDiff struct {
	canWrite func(string) bool
}

func (d streamPolicyDiff) require(section, path string) error {
	if d.canWrite(section) {
		return nil
	}
	return fmt.Errorf("plugin changed stream field %s without %s", path, section)
}

func (d streamPolicyDiff) topology(path string) error {
	return d.require(string(outboundpolicy.StreamTopologySection()), path)
}

// events compares the event sequence as a stream transaction.  Pairing by
// position preserves the meaning of a one-for-one text or tool-argument
// rewrite (semantic grant only).  Count/action changes are topology, and an
// otherwise identical multiset in a different order is topology as well.
// Fragment assembly necessarily changes count, so it charges topology plus the
// recursively observed semantic fields of the added/removed fragments.
func (d streamPolicyDiff) events(accepted, returned []*pbv2.StreamEvent, path string) error {
	if len(accepted) != len(returned) {
		if err := d.topology(path); err != nil {
			return err
		}
	}
	if sameEventMultiset(accepted, returned) && !sameEventOrder(accepted, returned) {
		if err := d.topology(path + " order"); err != nil {
			return err
		}
	}

	n := len(accepted)
	if len(returned) < n {
		n = len(returned)
	}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("%s[%d]", path, i)
		if err := d.event(accepted[i], returned[i], p); err != nil {
			return err
		}
	}
	for i := n; i < len(accepted); i++ {
		if err := d.eventMissing(accepted[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	for i := n; i < len(returned); i++ {
		if err := d.eventMissing(returned[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func sameEventOrder(a, b []*pbv2.StreamEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameEventMultiset(a, b []*pbv2.StreamEvent) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, ev := range a {
		wire, err := proto.Marshal(ev)
		if err != nil {
			return false
		}
		counts[string(wire)]++
	}
	for _, ev := range b {
		wire, err := proto.Marshal(ev)
		if err != nil || counts[string(wire)] == 0 {
			return false
		}
		counts[string(wire)]--
	}
	return true
}

func eventArm(ev *pbv2.StreamEvent) string {
	if ev == nil || ev.Event == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", ev.Event)
}

func (d streamPolicyDiff) event(accepted, returned *pbv2.StreamEvent, path string) error {
	if accepted == nil || returned == nil {
		if accepted == returned {
			return nil
		}
		if err := d.topology(path + " action"); err != nil {
			return err
		}
		if accepted != nil {
			if err := d.eventMissing(accepted, path); err != nil {
				return err
			}
		}
		if returned != nil {
			return d.eventMissing(returned, path)
		}
		return nil
	}
	if eventArm(accepted) != eventArm(returned) {
		if err := d.topology(path + " action"); err != nil {
			return err
		}
		if err := d.eventMissing(accepted, path); err != nil {
			return err
		}
		return d.eventMissing(returned, path)
	}
	return d.message(accepted.ProtoReflect(), returned.ProtoReflect(), path)
}

// eventMissing applies the registered policies to every material field in an
// added or removed event.  The caller has already charged the event action as
// topology; this recursive pass adds semantic grants and rejects host-owned
// facts carried by the event.
func (d streamPolicyDiff) eventMissing(ev *pbv2.StreamEvent, path string) error {
	if ev == nil {
		return nil
	}
	return d.messagePresent(ev.ProtoReflect(), path)
}

func (d streamPolicyDiff) message(a, b protoreflect.Message, path string) error {
	if !a.IsValid() || !b.IsValid() {
		if a.IsValid() == b.IsValid() {
			return nil
		}
		if a.IsValid() {
			return d.messagePresent(a, path)
		}
		return d.messagePresent(b, path)
	}
	if !bytes.Equal(a.GetUnknown(), b.GetUnknown()) {
		return fmt.Errorf("plugin changed unknown fields in stream %s", path)
	}
	name := a.Descriptor().FullName()
	if !outboundpolicy.OutboundMessageRegistered(name) {
		return fmt.Errorf("stream policy has no registered message %s at %s", name, path)
	}
	fields, _ := outboundpolicy.OutboundFieldNames(name)
	for _, field := range fields {
		fd := a.Descriptor().Fields().ByName(protoreflect.Name(field))
		if fd == nil { // outboundpolicy.Validate is the startup proof; defensive.
			return fmt.Errorf("stream policy field %s.%s is absent from the descriptor", name, field)
		}
		p, _ := outboundpolicy.OutboundFieldPolicy(name, field)
		if err := d.field(a, b, fd, p, path+"."+field); err != nil {
			return err
		}
	}
	return nil
}

func (d streamPolicyDiff) messagePresent(m protoreflect.Message, path string) error {
	if !m.IsValid() {
		return nil
	}
	if len(m.GetUnknown()) > 0 {
		return fmt.Errorf("plugin wrote unknown fields in stream %s", path)
	}
	name := m.Descriptor().FullName()
	if !outboundpolicy.OutboundMessageRegistered(name) {
		return fmt.Errorf("stream policy has no registered message %s at %s", name, path)
	}
	fields, _ := outboundpolicy.OutboundFieldNames(name)
	for _, field := range fields {
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(field))
		p, _ := outboundpolicy.OutboundFieldPolicy(name, field)
		if !fieldMaterial(m, fd) {
			continue
		}
		if err := d.changedPresent(m, fd, p, path+"."+field); err != nil {
			return err
		}
	}
	return nil
}

func fieldMaterial(m protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
	if fd.IsList() {
		return m.Get(fd).List().Len() != 0
	}
	if fd.HasPresence() {
		return m.Has(fd)
	}
	return !m.Get(fd).Equal(fd.Default())
}

func (d streamPolicyDiff) field(a, b protoreflect.Message, fd protoreflect.FieldDescriptor, p outboundpolicy.FieldPolicy, path string) error {
	pa, pb := fieldMaterial(a, fd), fieldMaterial(b, fd)
	if !pa && !pb {
		return nil
	}
	if pa != pb {
		if pa {
			return d.changedPresent(a, fd, p, path)
		}
		return d.changedPresent(b, fd, p, path)
	}
	if fieldEqual(a.Get(fd), b.Get(fd), fd) {
		return nil
	}
	return d.changedBoth(a.Get(fd), b.Get(fd), fd, p, path)
}

func fieldEqual(a, b protoreflect.Value, fd protoreflect.FieldDescriptor) bool {
	if fd.IsList() {
		al, bl := a.List(), b.List()
		if al.Len() != bl.Len() {
			return false
		}
		for i := 0; i < al.Len(); i++ {
			if !valueEqual(al.Get(i), bl.Get(i), fd) {
				return false
			}
		}
		return true
	}
	return valueEqual(a, b, fd)
}

func valueEqual(a, b protoreflect.Value, fd protoreflect.FieldDescriptor) bool {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return proto.Equal(a.Message().Interface(), b.Message().Interface())
	case protoreflect.BytesKind:
		return bytes.Equal(a.Bytes(), b.Bytes())
	default:
		return a.Equal(b)
	}
}

func (d streamPolicyDiff) changedPresent(m protoreflect.Message, fd protoreflect.FieldDescriptor, p outboundpolicy.FieldPolicy, path string) error {
	switch p.Kind() {
	case outboundpolicy.PolicyHostOwned:
		return fmt.Errorf("plugin changed host-owned stream field %s", path)
	case outboundpolicy.PolicyBoundSignature:
		return nil // verifyStream{Prefix} owns the whole-scope rule.
	case outboundpolicy.PolicySection, outboundpolicy.PolicyTopology:
		section, _ := p.Section()
		return d.require(string(section), path)
	case outboundpolicy.PolicyContainer, outboundpolicy.PolicyFixedContainer, outboundpolicy.PolicyDelegate:
		if fd.IsList() {
			for i := 0; i < m.Get(fd).List().Len(); i++ {
				if err := d.messagePresent(m.Get(fd).List().Get(i).Message(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
			return nil
		}
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			return d.messagePresent(m.Get(fd).Message(), path)
		}
	}
	return fmt.Errorf("stream policy cannot add/remove %s", path)
}

func (d streamPolicyDiff) changedBoth(a, b protoreflect.Value, fd protoreflect.FieldDescriptor, p outboundpolicy.FieldPolicy, path string) error {
	switch p.Kind() {
	case outboundpolicy.PolicyHostOwned:
		return fmt.Errorf("plugin changed host-owned stream field %s", path)
	case outboundpolicy.PolicyBoundSignature:
		return nil // verified transactionally by verifyStream{Prefix}.
	case outboundpolicy.PolicySection, outboundpolicy.PolicyTopology:
		// Message-valued section/topology fields charge their parent only
		// when presence or oneof selection changes.  With matching presence
		// recurse so a ContentBlockStart index remains topology while a
		// ToolCallRef name/arguments remains assistant semantics.
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			return d.message(a.Message(), b.Message(), path)
		}
		section, _ := p.Section()
		return d.require(string(section), path)
	case outboundpolicy.PolicyContainer:
		return d.container(a, b, fd, path, false)
	case outboundpolicy.PolicyFixedContainer:
		return d.container(a, b, fd, path, true)
	case outboundpolicy.PolicyDelegate:
		return d.container(a, b, fd, path, false)
	}
	return fmt.Errorf("stream policy has no mutation rule for %s", path)
}

func (d streamPolicyDiff) container(a, b protoreflect.Value, fd protoreflect.FieldDescriptor, path string, fixed bool) error {
	if fd.IsList() {
		al, bl := a.List(), b.List()
		if fixed && al.Len() != bl.Len() {
			return fmt.Errorf("plugin changed fixed stream container cardinality at %s", path)
		}
		n := al.Len()
		if bl.Len() < n {
			n = bl.Len()
		}
		for i := 0; i < n; i++ {
			if err := d.message(al.Get(i).Message(), bl.Get(i).Message(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		for i := n; i < al.Len(); i++ {
			if err := d.messagePresent(al.Get(i).Message(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		for i := n; i < bl.Len(); i++ {
			if err := d.messagePresent(bl.Get(i).Message(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		return d.message(a.Message(), b.Message(), path)
	}
	return fmt.Errorf("stream policy container %s is not a message", path)
}
