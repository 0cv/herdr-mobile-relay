const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-493-8be3ae94b05409d5/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
