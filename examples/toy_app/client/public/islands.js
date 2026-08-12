// The application's islands. The framework ships the registry, not the
// islands: howl.island(name, setup) hands a setup function the element the
// server already rendered, plus whatever props the markup declared.
//
// Loaded after /static/app.js, which is why howl exists by the time this runs.
// A registration arriving late still hydrates — the registry re-scans.

howl.island("counter", (el, props) => {
  let n = props.start ?? 0;
  const btn = el.querySelector("[data-count]");
  const label = props.label ?? "count";
  const paint = () => (btn.textContent = `${label}: ${n}`);
  btn.addEventListener("click", () => {
    n++;
    paint();
  });
  paint();
});

// Client-side filter + sort. 25 lines of vanilla, zero round-trips per
// keystroke.
howl.island("table-tools", (el) => {
  const body = el.parentElement.querySelector("[data-rows]");
  const input = el.querySelector("[data-filter]");
  const sortBtn = el.querySelector("[data-sort]");
  const rows = () => [...body.querySelectorAll("tr")];

  input.addEventListener("input", () => {
    const q = input.value.toLowerCase();
    for (const tr of rows()) tr.hidden = !tr.dataset.name.includes(q);
  });

  sortBtn.addEventListener("click", () => {
    const by = sortBtn.dataset.sort === "value" ? "name" : "value";
    sortBtn.dataset.sort = by;
    sortBtn.textContent = `sort: ${by}`;
    rows()
      .sort((a, b) =>
        by === "name"
          ? a.dataset.name.localeCompare(b.dataset.name)
          : +b.dataset.value - +a.dataset.value,
      )
      .forEach((tr) => body.appendChild(tr));
  });
});

howl.island("toggle", (el) => {
  const box = el.querySelector("[data-toggle]");
  const out = el.querySelector("[data-toggle-out]");
  box.addEventListener("change", () => (out.textContent = `value: ${box.checked}`));
});
