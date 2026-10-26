const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.2-475-accb5edbaaa354d4/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
