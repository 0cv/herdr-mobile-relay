const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-504-6b47817949189a6c/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
