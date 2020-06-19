# Configuration Files

## Allowed APIs

The **allowedAPIs.cfg**  gives the ability to allow specific APIs in the spec. The following allowedAPIs.cfg would generate tests for endpoints like: `/v1/users`, `/v1/users/{id}`, `/v1/users/{id}/forgotPassword` etc.

```yml
/v1/users
```

By default, if no allowedAPIs.cfg is provided, it is assumed that all the APIs have to be tested.

## Blacklisting Endpoints

The **ignorePaths.cfg** lets you blacklist specific endpoints of an API. It will only affect paths which are a direct match. For example, if the following path was blacklisted:

```yml
/v1/users/{id}
```

Tests for `/v1/users` and `/v1/users/{id}/forgotPassword` would be generated but not GET, PUT ones for `/v1/users/{id}`.

By default, no endpoints are blacklisted.

## Unique Keys

Fuzzing requires the key of the parameter which has to be unique for every request in order to prevent *duplicate_key* errors.

This list of unique fields can be provided as a list in **uniqueKeys.yml**:

```yml
uniqueKeys:
  - name
  - title
  - email
```

During fuzzing, along with the parameter being fuzzed, the above keys would be randomly generated.

## Meta

Fuzz failures would be logged to **mqfails.jsonl** and each line in it will be appended with additional meta data provided in **meta.yml**:

```yml
url: https://jenkins.com/job/jobName/buildNumber
build: 42
deploy: production
```

## Local Dataset

The **dataset.yml** has to be a structured yaml of the following format:

```yml
Positive:
  String:
    String1
    String2
  Integer:
    Int1
Negative:
  Integer:
    Int2
```
