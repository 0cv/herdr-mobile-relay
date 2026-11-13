const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-497-e26e46b6ac8125e1/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
