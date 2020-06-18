# Fuzzing

When the fuzzType parameter is ste, fuzzing will be done on requests having a body of type map/dict.

- A base request is duplicated
- Unique field (name/email) is replaced with a random
- A single body parameter’s value is switched to one from the dataset for every fuzzed request.

## Dataset

- Default positive strings dataset set to https://github.com/minimaxir/big-list-of-naughty-strings/ (~500 strings)
- Optional flag to use local dataset local dataset (yaml)
  
## Types of fuzzing

### Positive Fuzzing

- A value from the dataset is picked and request is sent
- Expectation is set to success along with regular checks/assertions
- The resource is deleted if it was a Create/POST request
- Only the base request is propagated to the next get, update, delete calls
- Create/POST requests are parallely executed
- Update/PUT requests are sequentially executed

### Data type fuzzing
  
- Integers, Floats, Bools will be fuzzed with Strings, Integers, Floats, Bools
- Strings cannot be type fuzzed as any value can be treated as a string
- Expectation is set to 400

### Negative fuzzing

- A negative value is picked from the dataset and request is sent
- Expectatiion is set to 400
- No need of any checks/assertions

Every fuzz value picked from the dataset is validated if any validations are provided in the schema like:

- Integers
  - Min value, Max value
- String
  - Regex
  - Min length, max length

## Batching

- Fuzz datasets can be large it's not feasible to execute tests on the entire dataset for every request.
- Only a subset of the dataset will bbe used in each run.
- Tracking is done via **.mqdata.yml** containing already fuzzed data which is updated on each run.

## Logging Failures

Any expectation mismatch will be logged to `.mqfails.jsonl` with the following info:

```json
{
    "endpoint": "/v1/users/{id}",
    "method": "PUT",
    "field": "name",
    "value": "J0hñ Døę",
    "expected": "success",
    "actual": "500 - Internal Server Error",
    "meta": {}
}
```

- These failures are also skipped in subsequent runs until resolved manually or resolved automatically on running the tool with the `repro` flag.
- Optional `repro` flag to run tests using these failing values in order to reproduce the issues
