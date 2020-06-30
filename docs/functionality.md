# Functionality

## Test Generator

- Parses the OpenAPI doc and groups related endpoints into test suites
- Endpoints in a test suite are sorted according to the following priority:
  - General endpoints (/users)
  - Object-specific crud (/users/{id})
  - Object-specific non-crud (/users/{id}/resetPassword)

  With a general priority of: POST, GET, PUT, DELETE

## Test Executor

- Goes through each endpoint in each test suite
- Uses parameters from static data (in the form of `params` or `meqa_init`) if provided else generates a random one by going through the schema
- Makes the corresponding request and receives the response
- Response is checked for the following assertions:
  - Status code - Expects a 2XX unless otherwise specified
  - Schema - The response should match the schema specified
  - Request/Response - Asserts if common fields between the request and response match
  - Across requests - Asserts if common objects between different responses of the same API match (ex. Create and read)
- Errors are reported accordingly and a summary is printed
- Results are written to a file along with the complete request and response parameters
