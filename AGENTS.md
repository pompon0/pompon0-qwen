golang rules:
* make code type-safe: use/introduce types that carry more information than the underlying primitive
* use structured concurrency: never spawn-and-forget goroutines, use utils/scope.Scope
  this does not apply within utils/, because utils/ provide primitives which need high performance.
* don't pass nil pointers as args, don't use nil pointers as field values (except for slices and immutable maps, for which nil is a valid value).
  Use utils.Option instead to represent optional values.
  It is OK to return "nil, err" in case the result should be discarded.
  Both Option\[T\] and (T,bool) are fine as return values.
  (T, bool, error) is discouraged, if it actually represents (Option\[T\],error)
* never remove comments if they still apply
* use modern golang syntax at all times
