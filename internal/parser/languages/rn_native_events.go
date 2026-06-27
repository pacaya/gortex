package languages

import (
	"regexp"

	"github.com/zzet/gortex/internal/parser"
)

// rnNativeEventTransport is the pub/sub transport label for a React Native
// native-module event channel: an Objective-C / Swift `sendEventWithName:`
// emit paired with a JS `NativeEventEmitter(...).addListener` handler. The
// `rn_` prefix marks it as a native cross-language bridge the event-channel
// synthesizer pairs (and the contracts broker layer ignores).
const rnNativeEventTransport = "rn_native_event"

// rnObjCSendEventRe matches an Objective-C `[self sendEventWithName:@"Name" …]`
// RCTEventEmitter emit, capturing the event name string.
var rnObjCSendEventRe = regexp.MustCompile(`sendEventWithName:\s*@"([^"]+)"`)

// rnSendEventWrapperRe matches a paren-form `sendEvent(...)` emit -- both the
// Swift labelled `sendEvent(withName: "Name", ...)` and a custom helper
// wrapper `sendEvent(reactContext, "Name", body)` -- capturing the first
// literal string argument as the event name. The `[^;{}]` guard keeps a
// single match from spanning a statement boundary.
var rnSendEventWrapperRe = regexp.MustCompile(`\bsendEvent\s*\([^;{}]*?"([^"]+)"`)

// mineRNNativeEmits scans native source for React Native event-emit sites and
// records one pub/sub publish per emit on the rn_native_event channel,
// attributed to the enclosing function via callerLookup. The event-channel
// synthesizer then pairs each native emit with the JS addListener handler on
// the same event name. File-scope emits (callerLookup returns "") are dropped.
func mineRNNativeEmits(src []byte, re *regexp.Regexp, callerLookup func(line int) string, filePath, language string, result *parser.ExtractionResult) {
	matches := re.FindAllSubmatchIndex(src, -1)
	if len(matches) == 0 {
		return
	}
	events := make([]pubsubEvent, 0, len(matches))
	for _, m := range matches {
		name := string(src[m[2]:m[3]])
		if name == "" {
			continue
		}
		events = append(events, pubsubEvent{
			role:      pubsubRolePublish,
			transport: rnNativeEventTransport,
			topic:     name,
			method:    "sendEventWithName",
			line:      lineAt(src, m[0]),
		})
	}
	emitPubsubEvents(events, callerLookup, filePath, language, result)
}

// rnJVMEmitRe matches an Android React Native device-event emit chain,
// reactContext.getJSModule(DeviceEventManagerModule.RCTDeviceEventEmitter::class.java).emit("Name", ...)
// (Kotlin) / ....RCTDeviceEventEmitter.class).emit("Name", ...) (Java), capturing
// the event name.
var rnJVMEmitRe = regexp.MustCompile(`getJSModule\s*\([^;]*?\)\s*\.\s*emit\s*\(\s*"([^"]+)"`)

// mineRNJVMEmits scans Java/Kotlin source for React Native native event emits --
// the getJSModule(...).emit("Name", ...) device-event-emitter chain and the
// common sendEvent(reactContext, "Name", ...) helper wrapper -- and records each
// as a publish on the rn_native_event channel. The two forms are syntactically
// disjoint, so neither double-counts the other.
func mineRNJVMEmits(src []byte, callerLookup func(line int) string, filePath, language string, result *parser.ExtractionResult) {
	mineRNNativeEmits(src, rnJVMEmitRe, callerLookup, filePath, language, result)
	mineRNNativeEmits(src, rnSendEventWrapperRe, callerLookup, filePath, language, result)
}
