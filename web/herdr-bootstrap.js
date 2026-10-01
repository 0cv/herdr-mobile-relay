const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.3-394-60cbed7a29f57bf2/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
