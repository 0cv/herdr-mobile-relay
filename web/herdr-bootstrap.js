const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-543-c9cb002ff6afbed9/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
