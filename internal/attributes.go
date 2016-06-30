package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/newrelic/go-agent/api"
	ats "github.com/newrelic/go-agent/api/attributes"
	"github.com/newrelic/go-agent/internal/jsonx"
)

// https://source.datanerd.us/agents/agent-specs/blob/master/Agent-Attributes-PORTED.md

type destinationSet int

const (
	destTxnEvent destinationSet = 1 << iota
	destError
	destTxnTrace
	destBrowser
)

const (
	destNone destinationSet = 0
	destAll  destinationSet = destTxnEvent | destTxnTrace | destError | destBrowser
)

const (
	attributeWildcardSuffix = '*'
)

type attributeModifier struct {
	match string // This will not contain a trailing '*'.
	includeExclude
}

type byMatch []*attributeModifier

func (m byMatch) Len() int           { return len(m) }
func (m byMatch) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }
func (m byMatch) Less(i, j int) bool { return m[i].match < m[j].match }

type attributeConfig struct {
	disabledDestinations destinationSet
	exactMatchModifiers  map[string]*attributeModifier
	// Once attributeConfig is constructed, wildcardModifiers is sorted in
	// lexicographical order.  Modifiers appearing later have precedence
	// over modifiers appearing earlier.
	wildcardModifiers []*attributeModifier
	agentDests        agentAttributeDests
}

type includeExclude struct {
	include destinationSet
	exclude destinationSet
}

func modifierApply(m *attributeModifier, d destinationSet) destinationSet {
	// Include before exclude, since exclude has priority.
	d |= m.include
	d &^= m.exclude
	return d
}

func applyAttributeConfig(c *attributeConfig, key string, d destinationSet) destinationSet {
	// Important: The wildcard modifiers must be applied before the exact
	// match modifiers, and the slice must be iterated in a forward
	// direction.
	for _, m := range c.wildcardModifiers {
		if strings.HasPrefix(key, m.match) {
			d = modifierApply(m, d)
		}
	}

	if m, ok := c.exactMatchModifiers[key]; ok {
		d = modifierApply(m, d)
	}

	d &^= c.disabledDestinations

	return d
}

func addModifier(c *attributeConfig, match string, d includeExclude) {
	if "" == match {
		return
	}
	exactMatch := true
	if attributeWildcardSuffix == match[len(match)-1] {
		exactMatch = false
		match = match[0 : len(match)-1]
	}
	mod := &attributeModifier{
		match:          match,
		includeExclude: d,
	}

	if exactMatch {
		if m, ok := c.exactMatchModifiers[mod.match]; ok {
			m.include |= mod.include
			m.exclude |= mod.exclude
		} else {
			c.exactMatchModifiers[mod.match] = mod
		}
	} else {
		for _, m := range c.wildcardModifiers {
			// Important: Duplicate entries for the same match
			// string would not work because exclude needs
			// precedence over include.
			if m.match == mod.match {
				m.include |= mod.include
				m.exclude |= mod.exclude
				return
			}
		}
		c.wildcardModifiers = append(c.wildcardModifiers, mod)
	}
}

func processDest(c *attributeConfig, dc *api.AttributeDestinationConfig, d destinationSet) {
	if !dc.Enabled {
		c.disabledDestinations |= d
	}
	for _, match := range dc.Include {
		addModifier(c, match, includeExclude{include: d})
	}
	for _, match := range dc.Exclude {
		addModifier(c, match, includeExclude{exclude: d})
	}
}

type attributeConfigInput struct {
	attributes        api.AttributeDestinationConfig
	errorCollector    api.AttributeDestinationConfig
	transactionEvents api.AttributeDestinationConfig
	browserMonitoring api.AttributeDestinationConfig
	transactionTracer api.AttributeDestinationConfig
}

var (
	sampleAttributeConfigInput = attributeConfigInput{
		attributes:        api.AttributeDestinationConfig{Enabled: true},
		errorCollector:    api.AttributeDestinationConfig{Enabled: true},
		transactionEvents: api.AttributeDestinationConfig{Enabled: true},
	}
)

func createAttributeConfig(input attributeConfigInput) *attributeConfig {
	c := &attributeConfig{
		exactMatchModifiers: make(map[string]*attributeModifier),
		wildcardModifiers:   make([]*attributeModifier, 0, 64),
	}

	processDest(c, &input.attributes, destAll)
	processDest(c, &input.errorCollector, destError)
	processDest(c, &input.transactionEvents, destTxnEvent)
	processDest(c, &input.transactionTracer, destTxnTrace)
	processDest(c, &input.browserMonitoring, destBrowser)

	sort.Sort(byMatch(c.wildcardModifiers))

	c.agentDests = calculateAgentAttributeDests(c)

	return c
}

type userAttribute struct {
	value interface{}
	dests destinationSet
}

type attributes struct {
	config *attributeConfig
	user   map[string]userAttribute
	agent  agentAttributes
}

// New agent attributes must be added in the following places:
// * attributes/attributes.go
// * agentAttributes
// * agentAttributeDests
// * calculateAgentAttributeDests
// * writeAgentAttributes

type agentAttributes struct {
	HostDisplayName              string
	RequestMethod                string
	RequestAcceptHeader          string
	RequestContentType           string
	RequestContentLength         int
	RequestHeadersHost           string
	RequestHeadersUserAgent      string
	RequestHeadersReferer        string
	ResponseHeadersContentType   string
	ResponseHeadersContentLength int
	ResponseCode                 string
}

type agentAttributeDests struct {
	HostDisplayName              destinationSet
	RequestMethod                destinationSet
	RequestAcceptHeader          destinationSet
	RequestContentType           destinationSet
	RequestContentLength         destinationSet
	RequestHeadersHost           destinationSet
	RequestHeadersUserAgent      destinationSet
	RequestHeadersReferer        destinationSet
	ResponseHeadersContentType   destinationSet
	ResponseHeadersContentLength destinationSet
	ResponseCode                 destinationSet
}

func calculateAgentAttributeDests(c *attributeConfig) agentAttributeDests {
	usual := destAll &^ destBrowser
	traces := destTxnTrace | destError
	return agentAttributeDests{
		HostDisplayName:              applyAttributeConfig(c, ats.HostDisplayName, usual),
		RequestMethod:                applyAttributeConfig(c, ats.RequestMethod, usual),
		RequestAcceptHeader:          applyAttributeConfig(c, ats.RequestAcceptHeader, usual),
		RequestContentType:           applyAttributeConfig(c, ats.RequestContentType, usual),
		RequestContentLength:         applyAttributeConfig(c, ats.RequestContentLength, usual),
		RequestHeadersHost:           applyAttributeConfig(c, ats.RequestHeadersHost, usual),
		RequestHeadersUserAgent:      applyAttributeConfig(c, ats.RequestHeadersUserAgent, traces),
		RequestHeadersReferer:        applyAttributeConfig(c, ats.RequestHeadersReferer, traces),
		ResponseHeadersContentType:   applyAttributeConfig(c, ats.ResponseHeadersContentType, usual),
		ResponseHeadersContentLength: applyAttributeConfig(c, ats.ResponseHeadersContentLength, usual),
		ResponseCode:                 applyAttributeConfig(c, ats.ResponseCode, usual),
	}
}

type agentAttributeWriter struct {
	needsComma bool
	buf        *bytes.Buffer
	d          destinationSet
}

func (w *agentAttributeWriter) writePrefix(name string, d destinationSet) bool {
	if 0 != w.d&d {
		if w.needsComma {
			w.buf.WriteByte(',')
		} else {
			w.needsComma = true
		}
		jsonx.AppendString(w.buf, name)
		w.buf.WriteByte(':')
		return true
	}
	return false
}

func (w *agentAttributeWriter) writeString(name string, val string, d destinationSet) {
	if "" != val && w.writePrefix(name, d) {
		jsonx.AppendString(w.buf, truncateStringValueIfLong(val))
	}
}

func (w *agentAttributeWriter) writeInt(name string, val int, d destinationSet) {
	if val >= 0 && w.writePrefix(name, d) {
		jsonx.AppendInt(w.buf, int64(val))
	}
}

func writeAgentAttributes(buf *bytes.Buffer, d destinationSet, values agentAttributes, dests agentAttributeDests) {
	w := &agentAttributeWriter{
		needsComma: false,
		buf:        buf,
		d:          d,
	}
	buf.WriteByte('{')
	w.writeString(ats.HostDisplayName, values.HostDisplayName, dests.HostDisplayName)
	w.writeString(ats.RequestMethod, values.RequestMethod, dests.RequestMethod)
	w.writeString(ats.RequestAcceptHeader, values.RequestAcceptHeader, dests.RequestAcceptHeader)
	w.writeString(ats.RequestContentType, values.RequestContentType, dests.RequestContentType)
	w.writeInt(ats.RequestContentLength, values.RequestContentLength, dests.RequestContentLength)
	w.writeString(ats.RequestHeadersHost, values.RequestHeadersHost, dests.RequestHeadersHost)
	w.writeString(ats.RequestHeadersUserAgent, values.RequestHeadersUserAgent, dests.RequestHeadersUserAgent)
	w.writeString(ats.RequestHeadersReferer, values.RequestHeadersReferer, dests.RequestHeadersReferer)
	w.writeString(ats.ResponseHeadersContentType, values.ResponseHeadersContentType, dests.ResponseHeadersContentType)
	w.writeInt(ats.ResponseHeadersContentLength, values.ResponseHeadersContentLength, dests.ResponseHeadersContentLength)
	w.writeString(ats.ResponseCode, values.ResponseCode, dests.ResponseCode)
	buf.WriteByte('}')
}

func newAttributes(config *attributeConfig) *attributes {
	return &attributes{
		config: config,
		agent: agentAttributes{
			RequestContentLength:         -1,
			ResponseHeadersContentLength: -1,
		},
	}
}

// ErrInvalidAttribute is returned when the value is not valid.
type ErrInvalidAttribute struct{ typeString string }

func (e ErrInvalidAttribute) Error() string {
	return fmt.Sprintf("attribute value type %s is invalid", e.typeString)
}

func valueIsValid(val interface{}) error {
	switch val.(type) {
	case string, bool, nil,
		uint8, uint16, uint32, uint64, int8, int16, int32, int64,
		float32, float64, uint, int, uintptr:
		return nil
	default:
		return ErrInvalidAttribute{
			typeString: fmt.Sprintf("%T", val),
		}
	}
}

type invalidAttributeKeyErr struct{ key string }

func (e invalidAttributeKeyErr) Error() string {
	return fmt.Sprintf("attribute key '%.32s...' exceeds length limit %d",
		e.key, attributeKeyLengthLimit)
}

type userAttributeLimitErr struct{ key string }

func (e userAttributeLimitErr) Error() string {
	return fmt.Sprintf("attribute '%s' discarded: limit of %d reached", e.key,
		attributeUserLimit)
}

func validAttributeKey(key string) error {
	// Attributes whose keys are excessively long are dropped rather than
	// truncated to avoid worrying about the application of configuration to
	// truncated values or performing the truncation after configuration.
	if len(key) > attributeKeyLengthLimit {
		return invalidAttributeKeyErr{key: key}
	}
	return nil
}

func truncateStringValueIfLong(val string) string {
	if len(val) > attributeValueLengthLimit {
		return stringLengthByteLimit(val, attributeValueLengthLimit)
	}
	return val
}

func truncateStringValueIfLongInterface(val interface{}) interface{} {
	if str, ok := val.(string); ok {
		val = interface{}(truncateStringValueIfLong(str))
	}
	return val
}

func addUserAttribute(a *attributes, key string, val interface{}, d destinationSet) error {
	val = truncateStringValueIfLongInterface(val)
	if err := valueIsValid(val); nil != err {
		return err
	}
	if err := validAttributeKey(key); nil != err {
		return err
	}
	dests := applyAttributeConfig(a.config, key, d)
	if destNone == dests {
		return nil
	}
	if nil == a.user {
		a.user = make(map[string]userAttribute)
	}

	if _, exists := a.user[key]; !exists && len(a.user) >= attributeUserLimit {
		return userAttributeLimitErr{key}
	}

	// Note: Duplicates are overridden: last attribute in wins.
	a.user[key] = userAttribute{
		value: val,
		dests: dests,
	}
	return nil
}

func writeAttributeValueJSON(buf *bytes.Buffer, val interface{}) {
	switch v := val.(type) {
	case nil:
		buf.WriteString(`null`)
	case string:
		jsonx.AppendString(buf, v)
	case bool:
		if v {
			buf.WriteString(`true`)
		} else {
			buf.WriteString(`false`)
		}
	case uint8:
		jsonx.AppendUint(buf, uint64(v))
	case uint16:
		jsonx.AppendUint(buf, uint64(v))
	case uint32:
		jsonx.AppendUint(buf, uint64(v))
	case uint64:
		jsonx.AppendUint(buf, v)
	case uint:
		jsonx.AppendUint(buf, uint64(v))
	case uintptr:
		jsonx.AppendUint(buf, uint64(v))
	case int8:
		jsonx.AppendInt(buf, int64(v))
	case int16:
		jsonx.AppendInt(buf, int64(v))
	case int32:
		jsonx.AppendInt(buf, int64(v))
	case int64:
		jsonx.AppendInt(buf, v)
	case int:
		jsonx.AppendInt(buf, int64(v))
	case float32:
		jsonx.AppendFloat(buf, float64(v))
	case float64:
		jsonx.AppendFloat(buf, v)
	default:
		jsonx.AppendString(buf, fmt.Sprintf("%T", v))
	}
}

func agentAttributesJSON(a *attributes, buf *bytes.Buffer, d destinationSet) {
	if nil == a {
		buf.WriteString("{}")
		return
	}
	writeAgentAttributes(buf, d, a.agent, a.config.agentDests)
}

func userAttributesJSON(a *attributes, buf *bytes.Buffer, d destinationSet) {
	buf.WriteByte('{')
	if nil != a {
		first := true
		for name, atr := range a.user {
			if 0 != atr.dests&d {
				if first {
					first = false
				} else {
					buf.WriteByte(',')
				}
				jsonx.AppendString(buf, name)
				buf.WriteByte(':')
				writeAttributeValueJSON(buf, atr.value)
			}
		}
	}
	buf.WriteByte('}')
}

func userAttributesStringJSON(a *attributes, d destinationSet) JSONString {
	if nil == a {
		return JSONString("{}")
	}
	estimate := len(a.user) * 128
	buf := bytes.NewBuffer(make([]byte, 0, estimate))
	userAttributesJSON(a, buf, d)
	bs := buf.Bytes()
	return JSONString(bs)
}

func agentAttributesStringJSON(a *attributes, d destinationSet) JSONString {
	if nil == a {
		return JSONString("{}")
	}
	estimate := 1024
	buf := bytes.NewBuffer(make([]byte, 0, estimate))
	agentAttributesJSON(a, buf, d)
	return JSONString(buf.Bytes())
}

func getUserAttributes(a *attributes, d destinationSet) map[string]interface{} {
	v := make(map[string]interface{})
	json.Unmarshal([]byte(userAttributesStringJSON(a, d)), &v)
	return v
}

func getAgentAttributes(a *attributes, d destinationSet) map[string]interface{} {
	v := make(map[string]interface{})
	json.Unmarshal([]byte(agentAttributesStringJSON(a, d)), &v)
	return v
}
