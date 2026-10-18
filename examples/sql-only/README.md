# SQL-only example

No Go code: a SQL schema, a config file, and the `subquery` binary.

## Run it

```sh
createdb app
psql -d app -f schema.sql

go build -o /tmp/subquery ../../cmd/subquery
/tmp/subquery -config subquery.conf
```

The server listens on `:3000`. After changing the schema, reload the cache:

```sh
psql -d app -c "notify pgrst, 'reload schema'"
```

## Query it

```sh
# all rows
curl -s localhost:3000/books

# vertical and horizontal filtering, ordering
curl -s 'localhost:3000/books?select=title,year&year=gt.1973&order=year.desc'

# a computed field, selectable and filterable like a column
curl -s 'localhost:3000/books?select=title,book_label'

# embedding through the foreign key, from both sides
curl -s 'localhost:3000/books?select=title,authors(name)'
curl -s 'localhost:3000/authors?select=name,books(title)'

# an aggregate with an implicit GROUP BY
curl -s 'localhost:3000/books?select=year,count()'

# the same object as CSV (plain columns, aliases, and casts)
curl -s -H 'Accept: text/csv' 'localhost:3000/books?select=id,title,year'

# an insert, then the RPC function
curl -s -X POST localhost:3000/books \
  -H 'Content-Type: application/json' \
  -d '{"title":"Cosmicomics","year":1965,"author_id":2}'
curl -s localhost:3000/rpc/best_seller
```

`GET /` serves a generated OpenAPI document describing all of the above.

## Next

- Add authentication: set `jwt-secret` and let a token's role claim drive
  `SET LOCAL ROLE`, so row-level security and grants apply per request.
- Use the [embedded example](../embedded) to run the same schema from Go.
