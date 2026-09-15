const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-382-d94d3346c9244736/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
