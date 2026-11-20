const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-516-d8b906b846a510e9/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
