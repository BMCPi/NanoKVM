(function () {
  'use strict';

  // Submit buttons that require input render disabled (button.Props.Disabled)
  // and are enabled here once their paired field has a value. The pairing is
  // declared in the markup — data-gate-input="#some-field" on the button —
  // rather than in a list kept here.
  //
  // That direction matters. This started as a hardcoded array of id pairs in
  // virtual_media.js, and the firmware panel was then written by copying the
  // markup pattern: its Upload button rendered disabled, nothing added it to
  // the array, and choosing a file left the button permanently unclickable.
  // A gate the button carries itself cannot be half-copied — if the attribute
  // travels with the markup, so does the behaviour.
  var SELECTOR = '[data-gate-input]';

  // syncGate reads live DOM state rather than trusting the event that
  // triggered it, so it is correct regardless of HOW the value got there.
  // 'input'/'change' cover typing and the native file picker, but several
  // real paths fill a field without firing either: bfcache/back-forward form
  // restoration (Chrome replays no events on pageshow), browser autofill in
  // some engines, and — notably on Linux — X11 middle-click primary-selection
  // paste, which has a history of landing the text without a trailing input
  // event depending on focus timing. A gate that only reacts to specific
  // events can show a filled field next to a permanently disabled button;
  // recomputing from .value on every plausible trigger makes it correct by
  // construction instead of by event-coverage luck.
  function syncGate(btn) {
    var sel = btn.getAttribute('data-gate-input');
    if (!sel) return;
    var input = document.querySelector(sel);
    // A gate whose input is absent leaves the button alone rather than
    // force-enabling it: a missing target is a markup bug, and enabling a
    // submit that has nothing to submit would turn it into a silent failure.
    if (input) btn.disabled = !input.value;
  }

  function syncAllGates() {
    document.querySelectorAll(SELECTOR).forEach(syncGate);
  }

  // Fan a field's change out to every button gated on it. Buttons are matched
  // by their declared selector actually selecting this element, so one field
  // may gate several buttons and a selector may be anything querySelector
  // accepts, not just an id.
  function syncGatesFor(target) {
    if (!target || !target.matches) return;
    document.querySelectorAll(SELECTOR).forEach(function (btn) {
      var sel = btn.getAttribute('data-gate-input');
      if (sel && target.matches(sel)) syncGate(btn);
    });
  }

  document.body.addEventListener('input', function (e) {
    syncGatesFor(e.target);
  });
  document.body.addEventListener('change', function (e) {
    syncGatesFor(e.target);
  });
  // Paste fires before the browser has inserted the text, so read after the
  // event loop turns over rather than off the event itself.
  document.body.addEventListener('paste', function (e) {
    setTimeout(function () { syncGatesFor(e.target); }, 0);
  });

  // Covers every path that fills a field without an event: the initial load,
  // an htmx swap that brings a form back with server-side (or
  // browser-restored) values already present, and returning to the page via
  // back/forward navigation (bfcache), which Chrome does not replay input
  // events for.
  syncAllGates();
  document.body.addEventListener('htmx:afterSwap', syncAllGates);
  window.addEventListener('pageshow', syncAllGates);
})();
