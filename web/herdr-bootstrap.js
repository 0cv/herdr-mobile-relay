const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-500-472af05ec87b916b/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
