const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.3-476-32215fb1dd3140be/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
