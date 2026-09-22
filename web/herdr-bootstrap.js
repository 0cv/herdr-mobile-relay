const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-391-70d86a1a42fb7f46/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
