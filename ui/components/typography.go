package components

// SectionLabelClass is the app's section-header type: the same treatment the
// settings dialog gets from field.Legend (text-xs, semibold, uppercase, muted,
// default tracking). Panels outside the field system — the navbar popups —
// spell their headers as raw markup, and hand-repeating the classes is what
// let them drift apart (11px here, 12px there; tracking-wide on one,
// tracking-wider on the next). One constant, usable on whatever element the
// surrounding markup calls for:
//
//	<p class={ SectionLabelClass }>Power</p>
//	<div class={ SectionLabelClass, "mb-2" }>[all]</div>
const SectionLabelClass = "text-xs font-semibold uppercase tracking-wider text-muted-foreground"
