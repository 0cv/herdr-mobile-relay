const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-498-35b8e1502a692cc9/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
