(() => {
  const savePreference = (key, value) => {
    try { localStorage.setItem(key, value); }
    catch (error) { console.warn("Preference could not be saved.", error); }
  };
  const readPreference = key => {
    try { return localStorage.getItem(key); }
    catch (error) { console.warn("Preference could not be read.", error); return null; }
  };
  const selectTheme = (theme = document.documentElement.dataset.theme || "system") => {
    const themeSwitch = document.querySelector(".theme-switch");
    if (themeSwitch) {
      themeSwitch.dataset.choice = theme;
      themeSwitch.querySelectorAll("button").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.themeChoice === theme)));
    }
    if (theme === "system") delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
  };
  selectTheme();
  document.addEventListener("click", event => {
    const button = event.target.closest("[data-theme-choice]");
    if (!button) return;
    selectTheme(button.dataset.themeChoice);
    savePreference("hc-theme", button.dataset.themeChoice);
  });
  const sort = document.getElementById("sort-select");
  if (sort) {
    const directionButton = document.getElementById("sort-direction");
    let direction = readPreference("hc-sort-direction") === "desc" ? -1 : 1;
    const filters = Array.from(document.querySelectorAll("[data-filter]"));
    const savedSort = readPreference("hc-sort");
    if (["state", "name", "due"].includes(savedSort)) sort.value = savedSort;
    let status = readPreference("hc-status") || "all";
    if (!filters.some(button => button.dataset.filter === status)) status = "all";
    const ranks = { down: 0, grace: 1, awaiting: 2, up: 3, paused: 4 };
    const names = new Intl.Collator("en", { numeric: true, sensitivity: "base" });
    const updateChecks = () => {
      const grid = document.querySelector(".check-grid");
      const cards = grid ? Array.from(grid.querySelectorAll(".check-card")) : [];
      cards.sort((a, b) => {
        let order = 0;
        if (sort.value === "state") order = ranks[a.dataset.status] - ranks[b.dataset.status];
        if (sort.value === "due") {
          // Checks without an active deadline stay last in either direction.
          const missing = Number(!a.dataset.due) - Number(!b.dataset.due);
          if (missing) return missing;
          if (a.dataset.due) order = Date.parse(a.dataset.due) - Date.parse(b.dataset.due);
        }
        return direction * (order || names.compare(a.dataset.name, b.dataset.name));
      });
      directionButton.dataset.direction = direction === 1 ? "asc" : "desc";
      const action = direction === 1 ? "Sort descending" : "Sort ascending";
      directionButton.setAttribute("aria-label", action);
      directionButton.dataset.full = action;
      cards.forEach((card, index) => {
        card.hidden = status !== "all" && card.dataset.status !== status;
        if (grid.children[index] !== card) grid.insertBefore(card, grid.children[index] || null);
      });
      document.querySelectorAll("[data-filter]").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.filter === status)));
      const empty = document.querySelector("#filter-empty");
      if (empty) empty.hidden = cards.some(card => !card.hidden);
    };
    sort.addEventListener("change", () => { savePreference("hc-sort", sort.value); updateChecks(); });
    directionButton.addEventListener("click", () => {
      direction *= -1;
      savePreference("hc-sort-direction", direction === 1 ? "asc" : "desc");
      updateChecks();
    });
    filters.forEach(button => button.addEventListener("click", () => {
      status = button.dataset.filter;
      savePreference("hc-status", status);
      updateChecks();
    }));
    updateChecks();
  }
  const formatter = new Intl.RelativeTimeFormat("en", { numeric: "always" });
  const fullTimeFormatter = new Intl.DateTimeFormat(undefined, {
    year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", second: "2-digit", timeZoneName: "short"
  });
  const updateTimes = () => {
    document.querySelectorAll("time[data-full]").forEach(element => {
      const date = new Date(element.dateTime);
      const fullTime = fullTimeFormatter.format(date);
      if (element.dataset.full !== fullTime) element.dataset.full = fullTime;
      if (element.getAttribute("aria-label") !== fullTime) element.setAttribute("aria-label", fullTime);
      const seconds = (date.getTime() - Date.now()) / 1000;
      const [size, unit] = Math.abs(seconds) >= 84600 ? [86400, "day"] : Math.abs(seconds) >= 3570 ? [3600, "hour"] : Math.abs(seconds) >= 60 ? [60, "minute"] : [1, "second"];
      const amount = Math.sign(seconds) * Math.round(Math.abs(seconds) / size);
      const text = amount === 0 ? "now" : formatter.format(amount, unit);
      const label = element.dataset.due === "true" ? text.replace(/ ago$/, " overdue") : text;
      if (element.textContent !== label) element.textContent = label;
    });
  };
  updateTimes();
  const positionTooltip = element => {
    const rect = element.getBoundingClientRect();
    const viewport = document.documentElement;
    element.style.setProperty("--tooltip-max-width", viewport.clientWidth - 32 + "px");
    const tooltip = element.matches(".check-link") ? document.getElementById(element.getAttribute("aria-describedby")) : element;
    const style = getComputedStyle(tooltip, tooltip === element ? "::after" : null);
    const width = parseFloat(style.width);
    const height = parseFloat(style.height);
    const left = Math.max(16, Math.min(rect.left, viewport.clientWidth - width - 16));
    const above = rect.top - height - 6;
    const top = Math.max(16, Math.min(above >= 16 ? above : rect.bottom + 6, viewport.clientHeight - height - 16));
    tooltip.style.setProperty("--tooltip-left", left + "px");
    tooltip.style.setProperty("--tooltip-top", top + "px");
    tooltip.dataset.positioned = "true";
  };
  for (const type of ["mouseover", "focusin"]) document.addEventListener(type, event => {
    const trigger = event.target.closest("[data-full], .check-link[aria-describedby]");
    if (trigger && !trigger.contains(event.relatedTarget)) positionTooltip(trigger);
  });
  for (const type of ["mouseout", "focusout"]) document.addEventListener(type, event => {
    const trigger = event.target.closest("[data-full], .check-name");
    if (!trigger || trigger.contains(event.relatedTarget)) return;
    const tooltip = trigger.matches(".check-name") ? trigger.querySelector(".description-tooltip") : trigger;
    if (tooltip && !trigger.contains(document.activeElement) && !trigger.matches(":hover")) delete tooltip.dataset.dismissed;
  });
  document.addEventListener("keydown", event => {
    if (event.key === "Escape") document.querySelectorAll("[data-full]:hover, [data-full]:focus, .check-name:hover .description-tooltip, .check-name:focus-within .description-tooltip").forEach(tooltip => { tooltip.dataset.dismissed = "true"; });
  });
  const positionVisibleTooltips = () => document.querySelectorAll("[data-full]:hover, [data-full]:focus, .check-link[aria-describedby]:hover, .check-link[aria-describedby]:focus").forEach(positionTooltip);
  window.addEventListener("resize", positionVisibleTooltips);
  window.addEventListener("scroll", positionVisibleTooltips, true);
  window.setInterval(updateTimes, 1000);

})();
