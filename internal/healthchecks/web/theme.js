(() => {
  try {
    const theme = localStorage.getItem("hc-theme");
    if (theme === "light" || theme === "dark") document.documentElement.dataset.theme = theme;
  } catch (error) {
    // Storage can be disabled by the browser; system colors still work.
    console.warn("Theme preference could not be read.", error);
  }
})();
