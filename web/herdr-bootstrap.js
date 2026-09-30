const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-383-95e2f6fdb3e33b8f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
