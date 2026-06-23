package languages

import (
	"strings"
	"sync"
)

// phpLaravelFacadesDefault maps a Laravel facade's bare class name to the bare
// class name of the backing service it proxies — the class whose method a
// `Facade::method()` static call actually invokes (`Cache::get()` forwards to
// the cache Repository's `get()`, not to a static `Cache::get`). The PHP
// extractor stamps the backing class as the call's receiver type so the
// resolver binds the facade call to that class's method instead of an ambiguous
// name-only match. Values are the method-bearing class for the facade's common
// surface (the Repository / Manager / Connection the accessor returns).
var phpLaravelFacadesDefault = map[string]string{
	"Cache":        "Repository",
	"Config":       "Repository",
	"DB":           "Connection",
	"Redis":        "Connection",
	"Route":        "Router",
	"Queue":        "Queue",
	"Session":      "Store",
	"Storage":      "FilesystemManager",
	"View":         "Factory",
	"Validator":    "Factory",
	"Mail":         "Mailer",
	"Event":        "Dispatcher",
	"Bus":          "Dispatcher",
	"Log":          "Logger",
	"Auth":         "AuthManager",
	"Gate":         "Gate",
	"Hash":         "HashManager",
	"URL":          "UrlGenerator",
	"Redirect":     "Redirector",
	"Request":      "Request",
	"Response":     "ResponseFactory",
	"Schema":       "Builder",
	"Artisan":      "Kernel",
	"Notification": "ChannelManager",
	"Password":     "PasswordBrokerManager",
	"Broadcast":    "BroadcastManager",
	"Cookie":       "CookieJar",
	"Crypt":        "Encrypter",
	"File":         "Filesystem",
	"Lang":         "Translator",
}

var (
	phpFacadeMu    sync.RWMutex
	phpFacadeExtra map[string]string
)

// RegisterPHPFacade registers (or overrides) a Laravel facade → backing class
// mapping so a project can teach the PHP extractor about custom facades from
// configuration. Both arguments are bare class names (no namespace). Safe for
// concurrent use; intended to be called at config-load time before indexing.
func RegisterPHPFacade(facade, backingClass string) {
	facade = strings.TrimSpace(facade)
	backingClass = strings.TrimSpace(backingClass)
	if facade == "" || backingClass == "" {
		return
	}
	phpFacadeMu.Lock()
	defer phpFacadeMu.Unlock()
	if phpFacadeExtra == nil {
		phpFacadeExtra = map[string]string{}
	}
	phpFacadeExtra[facade] = backingClass
}

// phpFacadeBackingClass returns the backing class for a facade name, preferring
// a config-registered override over the built-in table. The lookup key is the
// bare last segment of the scope (so `\Cache` and
// `Illuminate\Support\Facades\Cache` both resolve as `Cache`).
func phpFacadeBackingClass(scope string) (string, bool) {
	facade := strings.TrimSpace(scope)
	facade = strings.TrimPrefix(facade, "\\")
	if i := strings.LastIndex(facade, "\\"); i >= 0 {
		facade = facade[i+1:]
	}
	if facade == "" {
		return "", false
	}
	phpFacadeMu.RLock()
	if phpFacadeExtra != nil {
		if c, ok := phpFacadeExtra[facade]; ok {
			phpFacadeMu.RUnlock()
			return c, true
		}
	}
	phpFacadeMu.RUnlock()
	c, ok := phpLaravelFacadesDefault[facade]
	return c, ok
}
